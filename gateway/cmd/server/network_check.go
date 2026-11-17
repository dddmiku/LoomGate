// ═══ 更新日志 ═══
// 2026-11-17：新增无凭据网络只读自检，用于安卓实机证明应用内 Go 走 bionic DNS
// 且能用系统根证书完成 TLS 握手；不读取账号、不发模型请求、不写任何文件。
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"
)

// networkCheckHost 是自检目标。选上游真实域名（只做 DNS + TLS 握手，不发 HTTP
// 请求）：同一域名在 CN/global 配置下都会被网关实际使用，且它不依赖任何账号。
const networkCheckHost = "www.workbuddy.ai"

const networkCheckTimeout = 12 * time.Second

// networkCheckResult 只包含公开网络事实：域名、解析出的地址数量、TLS 版本与
// 证书有效期。故意不输出 IP 明细与证书正文，避免自检日志被当成网络环境指纹
// 或误当凭据输出。
type networkCheckResult struct {
	OK           bool   `json:"ok"`
	Host         string `json:"host"`
	DNSAddresses int    `json:"dns_addresses"`
	DNSError     string `json:"dns_error,omitempty"`
	TLSVersion   string `json:"tls_version,omitempty"`
	TLSNotAfter  string `json:"tls_not_after,omitempty"`
	TLSError     string `json:"tls_error,omitempty"`
	Note         string `json:"note"`
}

// isNetworkCheckArg 只接受精确的参数形态；任何多余参数都不触发自检，避免把
// 运维命令拼错当成"已自检"。
func isNetworkCheckArg(args []string) bool {
	return len(args) == 1 && args[0] == "--loomgate-network-check"
}

// runNetworkCheckIfRequested 处理 `--loomgate-network-check`，返回是否已处理。
// 挂在 main 读取配置之前：自检不能因为配置/账号/目录损坏而跑不起来，也不能
// 触碰任何持久化状态。
func runNetworkCheckIfRequested(args []string) bool {
	if !isNetworkCheckArg(args) {
		return false
	}
	code := runNetworkCheck(context.Background(), networkCheckHost, net.DefaultResolver.LookupHost, tlsHandshake, os.Stdout)
	if code != 0 {
		os.Exit(code)
	}
	return true
}

// logNetworkCheck 在网关进程内跑同一只读探针，只把公开事实写进日志。
// 安卓交付门用这一行作为"应用内"证据：它由 App 实际拉起的子进程打印，
// 证明的是运行中的网关自己的解析/TLS 路径，而不是设备 shell 的 curl/ping。
func logNetworkCheck(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, networkCheckTimeout)
	defer cancel()
	result := performNetworkCheck(ctx, networkCheckHost)
	raw, err := json.Marshal(result)
	if err != nil {
		log.Printf("WARN: [网络自检] 结果编码失败：%v", err)
		return
	}
	if result.OK {
		log.Printf("[网络自检] %s", raw)
		return
	}
	log.Printf("WARN: [网络自检] %s", raw)
}

func tlsHandshake(ctx context.Context, host string) (tls.ConnectionState, error) {
	dialer := &net.Dialer{Timeout: networkCheckTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, "443"), &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer conn.Close()
	return conn.ConnectionState(), nil
}

func performNetworkCheck(ctx context.Context, host string) networkCheckResult {
	return performNetworkCheckWith(ctx, host, net.DefaultResolver.LookupHost, tlsHandshake)
}

func performNetworkCheckWith(ctx context.Context, host string,
	lookup func(context.Context, string) ([]string, error),
	dial func(context.Context, string) (tls.ConnectionState, error)) networkCheckResult {
	result := networkCheckResult{
		Host: host,
		Note: "DNS and TLS only; no credentials, no HTTP request, no model call",
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		result.DNSError = err.Error()
		return result
	}
	result.DNSAddresses = len(addrs)
	state, err := dial(ctx, host)
	if err != nil {
		result.TLSError = err.Error()
		return result
	}
	result.OK = true
	result.TLSVersion = tlsVersionName(state.Version)
	if len(state.PeerCertificates) > 0 && !state.PeerCertificates[0].NotAfter.IsZero() {
		result.TLSNotAfter = state.PeerCertificates[0].NotAfter.UTC().Format(time.RFC3339)
	}
	return result
}

func runNetworkCheck(parent context.Context, host string,
	lookup func(context.Context, string) ([]string, error),
	dial func(context.Context, string) (tls.ConnectionState, error),
	out io.Writer) int {
	ctx, cancel := context.WithTimeout(parent, networkCheckTimeout)
	defer cancel()
	result := performNetworkCheckWith(ctx, host, lookup, dial)
	if err := json.NewEncoder(out).Encode(result); err != nil {
		fmt.Fprintf(out, "encode network check: %v\n", err)
		return 1
	}
	if !result.OK {
		return 1
	}
	return 0
}

func tlsVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	default:
		return fmt.Sprintf("0x%04x", version)
	}
}
