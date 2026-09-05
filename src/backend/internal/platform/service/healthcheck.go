package service

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const maximumHealthResponseBytes = 64

// CheckConfiguredHealth 读取健康服务端口并强制通过本机回环地址访问 ready 端点。
func CheckConfiguredHealth(defaultHTTPAddress string, timeout time.Duration) error {
	address := strings.TrimSpace(os.Getenv("MAIL_SUITE_HTTP_ADDRESS"))
	if address == "" {
		address = defaultHTTPAddress
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return errors.New("健康检查配置无效")
	}
	endpoint := "http://" + net.JoinHostPort("127.0.0.1", port) + "/health/ready"
	return CheckHealth(endpoint, timeout)
}

// CheckHealth 请求进程本机 ready 端点，供 scratch 容器的 Docker healthcheck 使用。
func CheckHealth(endpoint string, timeout time.Duration) error {
	if endpoint == "" || timeout <= 0 {
		return errors.New("健康检查配置无效")
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Get(endpoint)
	if err != nil {
		return errors.New("本机健康端点不可用")
	}
	defer func() { _ = response.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumHealthResponseBytes+1))
	if readErr != nil || response.StatusCode != http.StatusOK || len(body) > maximumHealthResponseBytes {
		return errors.New("本机健康端点未就绪")
	}
	return nil
}
