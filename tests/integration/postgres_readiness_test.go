package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// apiProcess 保存黑盒集成测试启动的 API 进程及其有界退出结果。
type apiProcess struct {
	command *exec.Cmd
	done    chan error
	output  *bytes.Buffer
}

// TestPostgreSQLReadinessCoversUnavailableAndRecoveredConnections 使用真实进程和 PostgreSQL 验证就绪闭环。
func TestPostgreSQLReadinessCoversUnavailableAndRecoveredConnections(t *testing.T) {
	databaseURL := os.Getenv("MAIL_SUITE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("未设置 MAIL_SUITE_TEST_DATABASE_URL，跳过真实 PostgreSQL 集成测试")
	}

	binaryPath := apiBinary(t)
	httpAddress := availableAddress(t)

	unavailable := startAPI(
		t,
		binaryPath,
		httpAddress,
		"postgres://mail_suite@127.0.0.1:1/mail_suite?sslmode=disable&connect_timeout=1",
	)
	waitForProbe(t, httpAddress, "/health/live", http.StatusOK, `{"status":"ok"}`)
	waitForProbe(
		t,
		httpAddress,
		"/health/ready",
		http.StatusServiceUnavailable,
		`{"status":"unavailable","code":"DEPENDENCY_UNAVAILABLE"}`,
	)
	stopAPI(t, unavailable)

	recovered := startAPI(t, binaryPath, httpAddress, databaseURL)
	t.Cleanup(func() { stopAPI(t, recovered) })
	waitForProbe(t, httpAddress, "/health/ready", http.StatusOK, `{"status":"ok"}`)
}

// apiBinary 返回 CI 预构建制品；本地显式运行集成测试时按需构建临时制品。
func apiBinary(t *testing.T) string {
	t.Helper()
	if configuredPath := strings.TrimSpace(os.Getenv("MAIL_SUITE_TEST_API_BINARY")); configuredPath != "" {
		return configuredPath
	}

	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("解析仓库根目录失败：%v", err)
	}
	binaryName := "mail-suite-api"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(t.TempDir(), binaryName)
	command := exec.Command("go", "build", "-o", binaryPath, "./src/backend/cmd/api")
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("构建 API 集成测试制品失败：%v\n%s", err, output)
	}
	return binaryPath
}

// availableAddress 预留一个本机环回地址，供随后启动的独立进程监听。
func availableAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("分配集成测试端口失败：%v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("释放集成测试端口失败：%v", err)
	}
	return address
}

// startAPI 以脱敏配置启动独立 API 进程，不继承可能冲突的 MAIL_SUITE_ 变量。
func startAPI(t *testing.T, binaryPath, httpAddress, databaseURL string) *apiProcess {
	t.Helper()
	output := &bytes.Buffer{}
	command := exec.Command(binaryPath)
	command.Env = append(
		withoutMailSuiteEnvironment(os.Environ()),
		"MAIL_SUITE_DATABASE_URL="+databaseURL,
		"MAIL_SUITE_HTTP_ADDRESS="+httpAddress,
		"MAIL_SUITE_PROBE_TIMEOUT=250ms",
		"MAIL_SUITE_SHUTDOWN_TIMEOUT=2s",
	)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("启动 API 集成测试进程失败：%v", err)
	}

	process := &apiProcess{command: command, done: make(chan error, 1), output: output}
	go func() { process.done <- command.Wait() }()
	return process
}

// stopAPI 请求正常终止并验证进程在约定时间内退出；不支持信号的平台回退为强制结束。
func stopAPI(t *testing.T, process *apiProcess) {
	t.Helper()
	if process == nil {
		return
	}
	forced := false
	if err := process.command.Process.Signal(os.Interrupt); err != nil {
		forced = true
		if killErr := process.command.Process.Kill(); killErr != nil {
			t.Fatalf("停止 API 集成测试进程失败：signal=%v kill=%v", err, killErr)
		}
	}

	select {
	case err := <-process.done:
		if err != nil && !forced {
			t.Fatalf("API 集成测试进程异常退出：%v\n%s", err, process.output.String())
		}
	case <-time.After(5 * time.Second):
		_ = process.command.Process.Kill()
		t.Fatalf("API 集成测试进程未在截止时间内退出\n%s", process.output.String())
	}
}

// waitForProbe 在有界时间内等待探针达到预期状态并核对稳定 JSON 契约。
func waitForProbe(t *testing.T, httpAddress, path string, expectedStatus int, expectedBody string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	url := "http://" + httpAddress + path
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var lastResult string
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("创建探针请求失败：%v", err)
		}
		response, err := client.Do(request)
		if err == nil {
			body := &bytes.Buffer{}
			_, copyErr := body.ReadFrom(response.Body)
			closeErr := response.Body.Close()
			lastResult = fmt.Sprintf("status=%d body=%s", response.StatusCode, body.String())
			if copyErr == nil && closeErr == nil &&
				response.StatusCode == expectedStatus &&
				body.String() == expectedBody &&
				response.Header.Get("Content-Type") == "application/json; charset=utf-8" {
				return
			}
		} else {
			lastResult = err.Error()
		}

		select {
		case <-ctx.Done():
			t.Fatalf("探针 %s 未达到预期状态：%s", path, lastResult)
		case <-ticker.C:
		}
	}
}

// withoutMailSuiteEnvironment 移除当前终端中的项目配置，保证测试输入唯一。
func withoutMailSuiteEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(strings.ToUpper(entry), "MAIL_SUITE_") {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
