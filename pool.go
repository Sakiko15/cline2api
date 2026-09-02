package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	pool     *AccountPool
	poolMu   sync.Mutex
	poolPath string
)

// rrCounter 是 round_robin 策略的全局选择计数器（P3-2）：跨过滤列表单调递增、
// 按当次列表长度取模，冷却集变化不再导致共享游标跳号。仅内存态，重启归零。
var rrCounter atomic.Uint64

func init() {
	poolPath = resolveDataPath(".cline-accounts.json")
}

// dataDirOverride 返回 CLINE_DATA_DIR 环境变量指定的数据目录（trim 后非空才有效）。
// 容器部署用它把整个数据目录作为一个 bind mount：单文件 bind mount 无法承载
// tmp+rename 原子写（Linux 不允许 rename 覆盖挂载点，EBUSY），目录挂载无此限制，
// 且宿主机目录不存在时 Docker 自动创建的类型恰好正确（零预创建文件）。
func dataDirOverride() string {
	return strings.TrimSpace(os.Getenv("CLINE_DATA_DIR"))
}

// resolveDataPath 按优先级查找数据文件：CLINE_DATA_DIR 目录 → exe 目录 →
// 工作目录 → 用户主目录。找到则用该路径（兼容旧版本在各候选位置存储的文件）；
// 都找不到则回退到 resolveDataDir()（环境变量目录优先，首次运行会在该位置创建）。
func resolveDataPath(filename string) string {
	// 0. CLINE_DATA_DIR 指定目录（容器部署整体挂载数据目录）
	if dir := dataDirOverride(); dir != "" {
		p := filepath.Join(dir, filename)
		if fileExists(p) {
			return p
		}
	}
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

// resolveDataDir 启动时确定数据目录（结果缓存）：CLINE_DATA_DIR 环境变量目录
// （允许创建）→ exe 目录 → cwd → ~/.cline2api，逐个探测可写性；全部不可写时
// 告警并回退 exe 目录（P2-15）。resolveDataPath 仅在所有候选路径都找不到既有
// 文件时才会走到这里。
var (
	resolveDataDirOnce sync.Once
	resolvedDataDir    string
)

func resolveDataDir() string {
	resolveDataDirOnce.Do(func() {
		// 环境变量目录最优先：Docker 部署整体挂载数据目录（容器内固定
		// CLINE_DATA_DIR=/app/data，见 Dockerfile）。不可用则告警后走默认链。
		if dir := dataDirOverride(); dir != "" {
			if d, ok := probeDataDir(dir); ok {
				resolvedDataDir = d
				return
			}
			log.Printf("WARNING: CLINE_DATA_DIR=%s is not writable, falling back to default candidates", dir)
		}
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
// rename 失败时降级保数据（单文件 bind mount 部署的 rename 会被内核以 EBUSY
// 拒绝——挂载点不可被 rename 覆盖；目录陷阱则把路径建成目录）：
//   - 目标为空目录 → 删除自愈后重试 rename（保持原子写）；
//   - 目标为非空目录 → 报错并提示手动清理；
//   - 其余（挂载点等）→ 原地写回同一 inode（放弃原子性，换取持久化生效）。
var inplaceWriteWarned sync.Map // path -> struct{}{}，原地写告警每路径只打一次

// osRenameFn 是 os.Rename 的包级 seam（互斥保护），供测试注入 rename 失败。
var (
	osRenameMu sync.Mutex
	osRenameFn = os.Rename
)

func renameFile(oldpath, newpath string) error {
	osRenameMu.Lock()
	defer osRenameMu.Unlock()
	return osRenameFn(oldpath, newpath)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := renameFile(tmp, path); err == nil {
		return nil
	}
	// rename 失败：先尝试目录陷阱自愈（Docker 把不存在的挂载路径建成空目录）
	if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
		if entries, _ := os.ReadDir(path); len(entries) == 0 {
			if rmErr := os.Remove(path); rmErr == nil {
				log.Printf("removed empty directory blocking data file: %s", path)
				if err := renameFile(tmp, path); err == nil {
					return nil
				}
			}
		} else {
			os.Remove(tmp)
			return fmt.Errorf("%s is a non-empty directory (Docker single-file mount trap); remove it on the host and switch to a directory mount (CLINE_DATA_DIR)", path)
		}
	}
	// 原地写兜底：直接写回挂载点文件（同一 inode，内核允许），放弃原子性
	if err := os.WriteFile(path, data, perm); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("atomic rename failed and in-place write failed: %w", err)
	}
	if _, dup := inplaceWriteWarned.LoadOrStore(path, struct{}{}); !dup {
		log.Printf("atomic rename unavailable for %s (single-file bind mount?); falling back to in-place write", path)
	}
	os.Remove(tmp)
	return nil
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

// checkDataPathMountTraps 启动时检测数据文件路径是否被 Docker 目录陷阱占用：
// 单文件 bind mount 且宿主机文件未预创建时，Docker 会把挂载路径自动建成目录，
// 此后所有落盘都会失败。给出可操作的修复提示，帮助老部署自诊。
func checkDataPathMountTraps() {
	for _, p := range []string{poolPath, requestLogsPath, zenConfigPath, proxyConfigFile} {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			log.Printf("ERROR: data file path %s is a directory (Docker single-file bind-mount trap); remove the directory on the host and mount the parent directory via CLINE_DATA_DIR instead (see docs/deploy-1panel.md)", p)
		}
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

// pruneOrphanModelCooldownsLocked 删除已不在模型列表中的冷却项（P3-14）：
// 模型同步替换/删除自定义模型后，残留在账号上的冷却项既不会过期清理入口也不会
// 再被命中，越积越多。调用方需持 poolMu（且已通过 loadPool 初始化全局池），
// 返回删除数；>0 时调用方 markPoolDirtyLocked() 或紧随 savePool()。
func pruneOrphanModelCooldownsLocked() int {
	if pool == nil {
		return 0
	}
	known := make(map[string]bool, len(pool.Models))
	for _, m := range pool.Models {
		known[m.ID] = true
	}
	pruned := 0
	for _, acc := range pool.Accounts {
		for mid := range acc.ModelCooldowns {
			if !known[mid] {
				delete(acc.ModelCooldowns, mid)
				pruned++
			}
		}
	}
	return pruned
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
	return pickAccountForModelWithFallbackExcept(model, fallbackToActive, nil)
}

// pickAccountForModelStrictExcept 在严格模式（无回退）基础上跳过 tried 中已试过的账号
// （P5-2：非 429 的 4xx 不产生冷却，free 链靠 tried 保证每账号每模型至多试一次，
// 否则 fill 策略会无限重选同一账号、round_robin 轮完复始形成死循环）。
func pickAccountForModelStrictExcept(model string, tried map[*Account]struct{}) *Account {
	return pickAccountForModelWithFallbackExcept(model, false, tried)
}

func pickAccountForModelWithFallbackExcept(model string, fallbackToActive bool, tried map[*Account]struct{}) *Account {
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

	// 该模型未冷却且未试过的账号列表
	eligible := make([]*Account, 0, len(active))
	for _, a := range active {
		if tried != nil {
			if _, skip := tried[a]; skip {
				continue
			}
		}
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
		// 全局单调计数器（P3-2）：与过滤列表长度解耦，冷却集变化不再跳号；
		// CurrentIdx 仅作管理端展示的最后命中下标。
		idx := int(rrCounter.Add(1)-1) % len(eligible)
		acc = eligible[idx]
		p.CurrentIdx = idx
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
		idx := int(rrCounter.Add(1)-1) % len(active)
		acc = active[idx]
		p.CurrentIdx = idx
	}
	markPoolDirtyLocked()
	return acc
}

// tokenFlight 账号级刷新单飞（P5-5）：cline refresh grant 会轮换 refresh token，
// 同账号并发两刷时第二个用旧 token → 4xx → 误标 expired 杀活账号。
// 键为 AccountID，refs 引用计数防误删（等待者排队期间条目不得被删）。
var (
	tokenFlightMu sync.Mutex
	tokenFlights  = map[string]*tokenFlight{}
)

type tokenFlight struct {
	mu   sync.Mutex
	refs int
}

// acquireTokenFlight 获取该账号的刷新锁，返回 release 函数。
// 锁序：flight 锁 → poolMu（refreshAccountToken 内部），全仓无反向路径。
func acquireTokenFlight(accountID string) func() {
	tokenFlightMu.Lock()
	fl := tokenFlights[accountID]
	if fl == nil {
		fl = &tokenFlight{}
		tokenFlights[accountID] = fl
	}
	fl.refs++
	tokenFlightMu.Unlock()

	fl.mu.Lock()

	return func() {
		fl.mu.Unlock()
		tokenFlightMu.Lock()
		fl.refs--
		if fl.refs == 0 {
			delete(tokenFlights, accountID)
		}
		tokenFlightMu.Unlock()
	}
}

// ensureAccountToken 返回可用访问令牌：新鲜则直接返回；否则经账号级单飞
// 刷新（P5-5：锁内快照消除数据竞争，单飞消除并发重复刷新）。注意
// refreshAccountToken 本体保持强制刷新语义，供管理端 refresh-all 与测试直调。
func ensureAccountToken(acc *Account) (string, error) {
	poolMu.Lock()
	token := acc.AccessToken
	expiresAt := acc.ExpiresAt
	poolMu.Unlock()
	if token != "" && time.Now().UnixMilli() < expiresAt {
		return token, nil
	}

	release := acquireTokenFlight(acc.AccountID)
	defer release()

	// 获锁后二次复查：等待期间已被并发刷新成功则直接复用（单飞去重）
	poolMu.Lock()
	token = acc.AccessToken
	expiresAt = acc.ExpiresAt
	poolMu.Unlock()
	if token != "" && time.Now().UnixMilli() < expiresAt {
		return token, nil
	}

	if err := refreshAccountToken(acc); err != nil {
		return "", err
	}

	poolMu.Lock()
	token = acc.AccessToken
	poolMu.Unlock()
	return token, nil
}

// snapshotPoolKeys 锁内拷贝 API 密钥列表（P5-9）：请求热路径与管理端并发
// 增删键时，无锁遍历 p.Keys 是数据竞争；恒时比较在快照上进行，语义不变。
func snapshotPoolKeys() []string {
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	keys := make([]string, len(p.Keys))
	copy(keys, p.Keys)
	return keys
}

// apiKeyValid 恒定时间比较且不提前结束，避免按命中时长泄漏 key 前缀（P2-3）。
func apiKeyValid(key string, keys []string) bool {
	valid := false
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			valid = true
		}
	}
	return valid
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
