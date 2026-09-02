package main

import (
	"bytes"
	"log"
	"os"
	"regexp"
	"strings"
	"testing"
)

// speedupPBKDF2 将 KDF 迭代临时调小（600k/次约 100-200ms，测试不需要），
// 返回恢复函数。
func speedupPBKDF2(t *testing.T) {
	t.Helper()
	old := adminPBKDF2Iterations
	adminPBKDF2Iterations = 1000
	t.Cleanup(func() { adminPBKDF2Iterations = old })
}

// captureAdminLogs 捕获标准 log 输出，返回读取缓冲与恢复函数。
func captureAdminLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// enableAutoPassword 在测试期间恢复随机生成开关（TestMain 全局关闭了它）。
func enableAutoPassword(t *testing.T) {
	t.Helper()
	old := adminAutoPassword
	adminAutoPassword = true
	t.Cleanup(func() { adminAutoPassword = old })
}

// TestBootstrapAdminPasswordGeneratesAndLogs：空哈希+无 env → 生成 PBKDF2
// 密码并打印日志，且日志中的密码串必须能通过登录校验（日志即真实密码）。
func TestBootstrapAdminPasswordGeneratesAndLogs(t *testing.T) {
	speedupPBKDF2(t)
	enableAutoPassword(t)
	if err := setAdminPassword(""); err != nil { // 重置为无密码态
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setAdminPassword("") })
	t.Setenv("CLINE_ADMIN_PASSWORD", "")

	logs := captureAdminLogs(t)
	bootstrapAdminPassword()

	hash := loadPool().AdminPasswordHash
	if !strings.HasPrefix(hash, adminPasswordHashPrefix) {
		t.Fatalf("password not generated: hash = %q", hash)
	}
	re := regexp.MustCompile(`initial password: ([0-9a-f]{8}(?:-[0-9a-f]{8}){3}) `)
	m := re.FindStringSubmatch(logs.String())
	if m == nil {
		t.Fatalf("initial password not logged:\n%s", logs.String())
	}
	if ok, _ := verifyAdminPassword(m[1]); !ok {
		t.Fatalf("logged password %q does not pass verification (log and hash mismatch)", m[1])
	}
	// 幂等：再次调用不覆盖、不重复打印
	bootstrapAdminPassword()
	if loadPool().AdminPasswordHash == "" {
		t.Fatal("hash unexpectedly cleared")
	}
	if newOne := re.FindStringSubmatch(logs.String()); newOne == nil || newOne[1] != m[1] {
		t.Fatalf("second bootstrap generated a new password or re-logged:\n%s", logs.String())
	}
}

// TestBootstrapAdminPasswordEnvTakesPrecedence：设置了环境变量时用它引导，
// 不随机生成。
func TestBootstrapAdminPasswordEnvTakesPrecedence(t *testing.T) {
	speedupPBKDF2(t)
	if err := setAdminPassword(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setAdminPassword("") })
	t.Setenv("CLINE_ADMIN_PASSWORD", "env-secret-pw")

	logs := captureAdminLogs(t)
	bootstrapAdminPassword()

	if ok, _ := verifyAdminPassword("env-secret-pw"); !ok {
		t.Fatalf("env password was not bootstrapped (hash=%q):\n%s", loadPool().AdminPasswordHash, logs.String())
	}
	if !strings.Contains(logs.String(), "bootstrapped from CLINE_ADMIN_PASSWORD environment variable") {
		t.Fatalf("env bootstrap log missing:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "initial password:") {
		t.Fatalf("random generation must not run when env password present:\n%s", logs.String())
	}
}

// TestBootstrapAdminPasswordExistingHashUntouched：已设密码的实例不受影响。
func TestBootstrapAdminPasswordExistingHashUntouched(t *testing.T) {
	speedupPBKDF2(t)
	if err := setAdminPassword("existing-pw"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setAdminPassword("") })
	legacy := loadPool().AdminPasswordHash

	logs := captureAdminLogs(t)
	bootstrapAdminPassword()

	if loadPool().AdminPasswordHash != legacy {
		t.Fatal("existing password hash was overwritten by bootstrap")
	}
	if ok, _ := verifyAdminPassword("existing-pw"); !ok {
		t.Fatal("existing password no longer verifies")
	}
	if strings.Contains(logs.String(), "initial password:") {
		t.Fatalf("bootstrap must be silent when password exists:\n%s", logs.String())
	}
}

// TestBootstrapAdminPasswordRNGFailureFailsClosed：随机源故障时保持无密码态
// （fail-closed 分支兜底拒绝非本机），不 panic、不写空哈希（对齐 P3-9）。
func TestBootstrapAdminPasswordRNGFailureFailsClosed(t *testing.T) {
	speedupPBKDF2(t)
	enableAutoPassword(t)
	if err := setAdminPassword(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setAdminPassword("") })
	t.Setenv("CLINE_ADMIN_PASSWORD", "")

	oldRand := cryptoRandRead
	cryptoRandRead = func(b []byte) (int, error) { return 0, os.ErrPermission }
	t.Cleanup(func() { cryptoRandRead = oldRand })

	logs := captureAdminLogs(t)
	bootstrapAdminPassword()

	if loadPool().AdminPasswordHash != "" {
		t.Fatalf("hash must stay empty on RNG failure, got %q", loadPool().AdminPasswordHash)
	}
	if !strings.Contains(logs.String(), "admin password generation failed") {
		t.Fatalf("failure log missing:\n%s", logs.String())
	}
}