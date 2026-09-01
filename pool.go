package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	pool     *AccountPool
	poolMu   sync.Mutex
	poolPath string
)

func init() {
	poolPath = resolveDataPath(".cline-accounts.json")
}

// resolveDataPath 按优先级查找数据文件：exe 目录 → 工作目录 → 用户主目录。
// 找到则用该路径（兼容旧版本在项目根目录存储的文件）；
// 都找不到则回退到 exe 目录（首次运行会在该位置创建）。
func resolveDataPath(filename string) string {
	// 1. exe 所在目录
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), filename)
		if fileExists(p) {
			return p
		}
	}
	// 2. 当前工作目录
	if pwd, err := os.Getwd(); err == nil {
		p := filepath.Join(pwd, filename)
		if fileExists(p) {
			return p
		}
	}
	// 3. 用户主目录下的 .cline2api/
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".cline2api", filename)
		if fileExists(p) {
			return p
		}
	}
	// 回退：启动时探测出的可写数据目录（exe 目录 → cwd → ~/.cline2api，P2-15）
	return filepath.Join(resolveDataDir(), filename)
}

// dirWritable 通过 CreateTemp 探测目录可写性。
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".cline-writability-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// probeDataDir 确保目录存在（0700）且可写。
func probeDataDir(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", false
	}
	if !dirWritable(dir) {
		return "", false
	}
	return dir, true
}

// resolveDataDir 启动时确定数据目录（结果缓存）：exe 目录 → cwd → ~/.cline2api
// （允许创建），逐个探测可写性；全部不可写时告警并回退 exe 目录（P2-15）。
// resolveDataPath 仅在三个候选路径都找不到既有文件时才会走到这里。
var (
	resolveDataDirOnce sync.Once
	resolvedDataDir    string
)

func resolveDataDir() string {
	resolveDataDirOnce.Do(func() {
		var candidates []string
		if exe, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Dir(exe))
		}
		if pwd, err := os.Getwd(); err == nil {
			candidates = append(candidates, pwd)
		}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, ".cline2api"))
		}
		for i, dir := range candidates {
			if d, ok := probeDataDir(dir); ok {
				resolvedDataDir = d
				if i > 0 {
					log.Printf("data dir: using %s (earlier candidate dirs are not writable)", d)
				}
				return
			}
		}
		if exe, err := os.Executable(); err == nil {
			resolvedDataDir = filepath.Dir(exe)
		} else {
			resolvedDataDir, _ = os.Getwd()
		}
		log.Printf("WARNING: no writable data dir among candidates; falling back to %s (writes may fail)", resolvedDataDir)
	})
	return resolvedDataDir
}

func loadPool() *AccountPool {
	poolMu.Lock()
	defer poolMu.Unlock()

	if pool != nil {
		return pool
	}

	data, err := os.ReadFile(poolPath)
	if err != nil {
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}
		return pool
	}

	var p AccountPool
	if err := json.Unmarshal(data, &p); err != nil {
		// 坏文件改名隔离：否则空池会在下一次 savePool 时覆盖销毁原始数据
		quarantineFile(poolPath, err)
		pool = &AccountPool{Accounts: []*Account{}, Keys: []string{}, Models: []Model{}}
		return pool
	}

	if p.Accounts == nil {
		p.Accounts = []*Account{}
	}
	if p.Keys == nil {
		p.Keys = []string{}
	}
	if p.Models == nil {
		p.Models = []Model{}
	}
	pool = &p
	return pool
}

// savePool 在未持有 poolMu 时使用：取锁后持久化。
// 注意：已持有 poolMu 的调用方必须改调 savePoolLocked()，否则死锁。
func savePool() {
	poolMu.Lock()
	defer poolMu.Unlock()
	savePoolLocked()
}

// writeFileAtomic 先写临时文件再 rename 替换，避免进程被杀/断电产生半截文件
// （pool / credentials / proxy+zen config / request logs 共用，P2-14）。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// quarantineFile 解析失败的数据文件改名隔离：否则默认值配置会在下一次保存时
// 直接覆盖销毁原始数据（用户失去排查线索）。隔离失败仅记录。
func quarantineFile(path string, cause error) {
	if renameErr := os.Rename(path, path+".bad"); renameErr == nil {
		log.Printf("%s corrupt, quarantined as %s.bad: %v", path, path, cause)
	} else {
		log.Printf("%s parse failed (quarantine failed: %v): %v", path, renameErr, cause)
	}
}

// savePoolLocked 在已持有 poolMu 的前提下持久化池（tmp+rename 原子写）。
// Marshal 必须在锁内进行：否则与并发修改 ModelStats/ModelCooldowns 等映射产生
// "concurrent map iteration and map write" 致命错误（进程直接退出，无法 recover）。
// writePath 固定为 path+".tmp"（writeFileAtomic），同路径并发写会交错损坏——
// 因此所有对 poolPath 的落盘（含 flushPoolDirty 后台协程）都必须在 poolMu 内串行。
func savePoolLocked() {
	data, err := json.MarshalIndent(pool, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal accounts: %v", err)
		return
	}
	if err := writeFileAtomic(poolPath, data, 0600); err != nil {
		// 写失败保留脏标记，交给 flushPoolDirty 下轮重试（P3-1：同步写失败不再丢数据）
		log.Printf("Failed to save accounts: %v", err)
		return
	}
	poolDirty = false
}

// poolDirty 与 poolDebouncer 支撑高频路径（pick/用量/冷却）的防抖落盘（P3-1）：
// 变更方置脏，后台 1s 内合并写一次；管理操作仍走 savePool 同步写。
// poolDirty 只能在持有 poolMu 时读写。进程被强杀最多丢 1s 的用量/冷却/游标。
var (
	poolDirty     bool
	poolDebouncer = newDebouncedFlush(time.Second, flushPoolDirty)
)

// markPoolDirtyLocked 已持有 poolMu 的高频路径使用（pick 等）。
func markPoolDirtyLocked() {
	poolDirty = true
	poolDebouncer.schedule()
}

// markPoolDirty 未持锁路径使用。
func markPoolDirty() {
	poolMu.Lock()
	markPoolDirtyLocked()
	poolMu.Unlock()
}

// flushPoolDirty 由 poolDebouncer 触发：锁内读脏→清脏→落盘（与同步写串行，
// 见 savePoolLocked 注释）。无变更时空转。
func flushPoolDirty() {
	poolMu.Lock()
	defer poolMu.Unlock()
	if !poolDirty {
		return
	}
	savePoolLocked()
}

// markAccount* 系列：账号状态字段的写操作必须持 poolMu——
// 与选择器/后台协程对 Accounts 的持锁遍历并发，无锁直写是数据竞争（P1-5）。

// markAccountCooldown 将账号置为冷却态（传输错误/无模型名 429）。
func markAccountCooldown(acc *Account, until time.Time) {
	if acc == nil {
		return
	}
	poolMu.Lock()
	acc.Status = "cooldown"
	acc.CooldownUntil = until
	markPoolDirtyLocked()
	poolMu.Unlock()
}

// markAccountExpired 将账号置为过期态（token 刷新被拒/二次 401）。
func markAccountExpired(acc *Account) {
	if acc == nil {
		return
	}
	poolMu.Lock()
	acc.Status = "expired"
	markPoolDirtyLocked()
	poolMu.Unlock()
}

// markAccountActive 将账号恢复为可用（重置/探活成功/刷新成功）。
func markAccountActive(acc *Account) {
	if acc == nil {
		return
	}
	poolMu.Lock()
	acc.Status = "active"
	markPoolDirtyLocked()
	poolMu.Unlock()
}

// markAccountUsed 记录一次成功使用（LastUsed/UsageCount），持锁自增。
func markAccountUsed(acc *Account) {
	if acc == nil {
		return
	}
	poolMu.Lock()
	acc.LastUsed = time.Now()
	acc.UsageCount++
	markPoolDirtyLocked()
	poolMu.Unlock()
}

func addAccount(acc *Account) {
	p := loadPool()
	poolMu.Lock()
	p.Accounts = append(p.Accounts, acc)
	poolMu.Unlock()
	savePool()
}

func removeAccount(accountID string) bool {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for i, a := range p.Accounts {
		if a.AccountID == accountID {
			p.Accounts = append(p.Accounts[:i], p.Accounts[i+1:]...)
			savePoolLocked()
			return true
		}
	}
	return false
}

func getAccountByID(accountID string) *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	for _, a := range p.Accounts {
		if a.AccountID == accountID {
			return a
		}
	}
	return nil
}

func refreshAccountToken(acc *Account) error {
	// 网络调用不持 poolMu（调用方可能在锁外批量刷新，见 handleAdminRefreshAll）
	resp, err := refreshClineToken(acc.RefreshToken)
	if err != nil {
		// 仅上游明确拒绝（4xx）才是账号级终态；网络抖动/5xx 属暂态失败，
		// 保持账号可用，由后续请求重试（P2-6：此前一次抖动即永久失效）
		var rej *tokenRefreshRejectedError
		if errors.As(err, &rej) {
			markAccountExpired(acc)
		}
		return fmt.Errorf("token refresh failed: %w", err)
	}

	poolMu.Lock()
	acc.AccessToken = "workos:" + resp.Data.AccessToken
	if resp.Data.RefreshToken != "" {
		acc.RefreshToken = resp.Data.RefreshToken
	}
	acc.ExpiresAt = parseExpiry(resp.Data.ExpiresAt) - 60000
	acc.Status = "active"
	poolMu.Unlock()
	savePool()
	return nil
}

func pickAccount() *Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	return pickAccountLocked(p)
}

// pickAccountForModel 按轮询/策略挑选一个「该模型未处于模型级冷却」的账号；
// 所有 active 账号对该模型都冷却时回退到普通 pickAccount（请求会得到模型级 429 提示）。
// 空模型名等同于 pickAccount。
func pickAccountForModel(model string) *Account {
	return pickAccountForModelWithFallback(model, true)
}

func pickAccountForModelStrict(model string) *Account {
	return pickAccountForModelWithFallback(model, false)
}

func pickAccountForModelWithFallback(model string, fallbackToActive bool) *Account {
	if model == "" {
		return pickAccount()
	}

	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	active := make([]*Account, 0)
	for _, a := range p.Accounts {
		if a.Status == "active" {
			active = append(active, a)
		}
	}
	if len(active) == 0 {
		return nil
	}

	// 该模型未冷却的账号列表
	eligible := make([]*Account, 0, len(active))
	for _, a := range active {
		until, cool := a.ModelCooldowns[model]
		if !cool || time.Now().After(until) {
			if cool {
				delete(a.ModelCooldowns, model)
			}
			eligible = append(eligible, a)
		}
	}

	if len(eligible) == 0 {
		if fallbackToActive {
			return pickAccountLocked(p)
		}
		return nil
	}

	cfg := getProxyConfig()
	var acc *Account
	switch cfg.Strategy {
	case "fill":
		acc = eligible[0]
	case "random":
		n := time.Now().UnixNano() % int64(len(eligible))
		acc = eligible[n]
	default: // round_robin
		if p.CurrentIdx >= len(eligible) {
			p.CurrentIdx = 0
		}
		acc = eligible[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(eligible)
	}
	markPoolDirtyLocked()
	return acc
}

// pickAccountLocked 在已持有 poolMu 的前提下执行普通轮询挑选（供 pickAccountForModel 回退用）。
func pickAccountLocked(p *AccountPool) *Account {
	active := make([]*Account, 0)
	for _, a := range p.Accounts {
		if a.Status == "active" {
			active = append(active, a)
		}
	}
	if len(active) == 0 {
		return nil
	}
	cfg := getProxyConfig()
	var acc *Account
	switch cfg.Strategy {
	case "fill":
		acc = active[0]
	case "random":
		n := time.Now().UnixNano() % int64(len(active))
		acc = active[n]
	default:
		if p.CurrentIdx >= len(active) {
			p.CurrentIdx = 0
		}
		acc = active[p.CurrentIdx]
		p.CurrentIdx = (p.CurrentIdx + 1) % len(active)
	}
	markPoolDirtyLocked()
	return acc
}

func ensureAccountToken(acc *Account) (string, error) {
	if acc.AccessToken != "" && time.Now().UnixMilli() < acc.ExpiresAt {
		return acc.AccessToken, nil
	}

	if err := refreshAccountToken(acc); err != nil {
		return "", err
	}

	return acc.AccessToken, nil
}

func listAccounts() []*Account {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()

	result := make([]*Account, len(p.Accounts))
	for i, a := range p.Accounts {
		// Don't expose tokens
		cp := &Account{
			AccountID:        a.AccountID,
			Email:            a.Email,
			Status:           a.Status,
			CooldownUntil:    a.CooldownUntil,
			LastUsed:         a.LastUsed,
			UsageCount:       a.UsageCount,
			PromptTokens:     a.PromptTokens,
			CompletionTokens: a.CompletionTokens,
			TotalTokens:      a.TotalTokens,
			CachedTokens:     a.CachedTokens,
			CreatedAt:        a.CreatedAt,
		}
		// 按模型细分统计（脱敏拷贝）
		if len(a.ModelStats) > 0 {
			cp.ModelStats = make(map[string]*ModelStat, len(a.ModelStats))
			for mid, st := range a.ModelStats {
				sc := *st
				cp.ModelStats[mid] = &sc
			}
		}
		// 模型级冷却（脱敏拷贝）
		if len(a.ModelCooldowns) > 0 {
			cp.ModelCooldowns = make(map[string]time.Time, len(a.ModelCooldowns))
			for mid, until := range a.ModelCooldowns {
				cp.ModelCooldowns[mid] = until
			}
		}
		result[i] = cp
	}
	return result
}

func addAccountFromDeviceAuth() (*Account, error) {
	fmt.Println()
	fmt.Println("=== Add New Cline Account (OAuth) ===")
	fmt.Println()

	device, err := workosDeviceAuth()
	if err != nil {
		return nil, err
	}

	authURL := device.VerificationURIComplete
	if authURL == "" {
		authURL = device.VerificationURI
	}

	fmt.Println("  1. Open this URL in your browser:")
	fmt.Println("     " + authURL)
	fmt.Println("  2. Enter code: " + device.UserCode)
	fmt.Println("  3. Log in with Google, GitHub, or email")
	fmt.Println()

	_ = openBrowser(authURL)
	fmt.Println("  Waiting for authorization...")

	interval := device.Interval
	if interval < 5 {
		interval = 5
	}
	expiresIn := device.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}

	workosTok, err := pollWorkosToken(device.DeviceCode, interval, expiresIn)
	if err != nil {
		return nil, err
	}

	fmt.Println("  WorkOS authorized. Registering with Cline...")

	cline, err := registerWithCline(workosTok.AccessToken, workosTok.RefreshToken)
	if err != nil {
		return nil, err
	}

	if cline.Data.RefreshToken == "" {
		return nil, fmt.Errorf("cline registration missing refresh token")
	}

	email := "unknown"
	if cline.Data.UserInfo != nil && cline.Data.UserInfo.Email != "" {
		email = cline.Data.UserInfo.Email
	}

	acc := &Account{
		AccountID:    fmt.Sprintf("acc_%d", time.Now().UnixMilli()),
		Email:        email,
		RefreshToken: cline.Data.RefreshToken,
		AccessToken:  "workos:" + cline.Data.AccessToken,
		ExpiresAt:    parseExpiry(cline.Data.ExpiresAt) - 60000,
		Status:       "active",
		CreatedAt:    time.Now(),
	}

	addAccount(acc)
	fmt.Printf("  Account added! Email: %s\n", email)
	return acc, nil
}
