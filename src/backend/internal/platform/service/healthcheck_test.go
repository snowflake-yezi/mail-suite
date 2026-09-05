package service

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// TestCheckConfiguredHealthUsesConfiguredPort 验证容器探针使用覆盖端口但不信任监听 host。
func TestCheckConfiguredHealthUsesConfiguredPort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/health/ready" {
			t.Fatalf("健康检查路径错误：%s", request.URL.Path)
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("解析测试服务地址失败：%v", err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("解析测试服务端口失败：%v", err)
	}
	t.Setenv("MAIL_SUITE_HTTP_ADDRESS", net.JoinHostPort("0.0.0.0", port))
	if err := CheckConfiguredHealth("127.0.0.1:1", time.Second); err != nil {
		t.Fatalf("配置端口上的 ready 响应应通过：%v", err)
	}
}

// TestCheckHealthAcceptsOnlyBoundedSuccess 验证本机探针只接受有界 200 响应。
func TestCheckHealthAcceptsOnlyBoundedSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/health/ready" {
			t.Fatalf("健康检查路径错误：%s", request.URL.Path)
		}
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	if err := CheckHealth(server.URL+"/health/ready", time.Second); err != nil {
		t.Fatalf("有效 ready 响应应通过：%v", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "unavailable", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	if err := CheckHealth(failing.URL, time.Second); err == nil {
		t.Fatal("非 200 ready 响应不应通过")
	}
}

// TestCheckHealthRejectsRedirectAndOversizedBody 验证探针不会跟随跳转或接受异常响应体。
func TestCheckHealthRejectsRedirectAndOversizedBody(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, "/other", http.StatusFound)
	}))
	defer redirect.Close()
	if err := CheckHealth(redirect.URL, time.Second); err == nil {
		t.Fatal("健康检查不得跟随重定向")
	}

	oversized := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write(make([]byte, maximumHealthResponseBytes+1))
	}))
	defer oversized.Close()
	if err := CheckHealth(oversized.URL, time.Second); err == nil {
		t.Fatal("超出边界的健康响应不应通过")
	}
}

// TestCheckConfiguredHealthRejectsInvalidAddress 验证探针拒绝无法提取端口的监听地址。
func TestCheckConfiguredHealthRejectsInvalidAddress(t *testing.T) {
	t.Setenv("MAIL_SUITE_HTTP_ADDRESS", "invalid")
	if err := CheckConfiguredHealth("127.0.0.1:8080", time.Second); err == nil {
		t.Fatal("无效监听地址不应触发健康请求")
	}
}
