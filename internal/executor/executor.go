// Package executor 由后端代发 HTTP 请求，规避浏览器 CORS 与受限头限制。
package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"interfacego/internal/model"
)

// maxBodyBytes 响应体最多记录的字节数，超出截断，防止超大响应撑爆内存与数据文件。
const maxBodyBytes = 5 << 20 // 5MB

// Execute 执行一次请求。
func Execute(ctx context.Context, req model.Request) model.Response {
	start := time.Now()
	resp := model.Response{Headers: []model.KV{}}

	rawURL := strings.TrimSpace(req.URL)
	if rawURL == "" {
		resp.Error = "请求地址不能为空"
		return resp
	}

	// 路径参数替换：{id} -> 实际值
	for _, kv := range req.PathVars {
		if !kv.Enabled || kv.Key == "" {
			continue
		}
		rawURL = strings.ReplaceAll(rawURL, "{"+kv.Key+"}", url.PathEscape(kv.Value))
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "http://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		resp.Error = "请求地址格式错误：" + err.Error()
		return resp
	}

	// 合并 query 参数
	q := u.Query()
	for _, kv := range req.Query {
		if !kv.Enabled || kv.Key == "" {
			continue
		}
		q.Set(kv.Key, kv.Value)
	}
	u.RawQuery = q.Encode()

	// 组装 body
	var bodyReader io.Reader
	autoContentType := ""
	switch req.BodyMode {
	case model.BodyJSON:
		if req.Body != "" {
			bodyReader = strings.NewReader(req.Body)
			autoContentType = "application/json; charset=utf-8"
		}
	case model.BodyRaw:
		if req.Body != "" {
			bodyReader = strings.NewReader(req.Body)
			autoContentType = req.ContentType
		}
	case model.BodyURLEncoded:
		vals := url.Values{}
		for _, kv := range req.FormData {
			if kv.Enabled && kv.Key != "" {
				vals.Add(kv.Key, kv.Value)
			}
		}
		bodyReader = strings.NewReader(vals.Encode())
		autoContentType = "application/x-www-form-urlencoded"
	case model.BodyForm:
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for _, kv := range req.FormData {
			if kv.Enabled && kv.Key != "" {
				_ = w.WriteField(kv.Key, kv.Value)
			}
		}
		_ = w.Close()
		bodyReader = &buf
		autoContentType = w.FormDataContentType()
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		resp.Error = "构造请求失败：" + err.Error()
		return resp
	}

	// 请求头
	for _, kv := range req.Headers {
		if !kv.Enabled || kv.Key == "" {
			continue
		}
		switch {
		case strings.EqualFold(kv.Key, "Host"):
			httpReq.Host = kv.Value
		case strings.EqualFold(kv.Key, "Content-Length"):
			// 交给 Transport 计算
		default:
			httpReq.Header.Set(kv.Key, kv.Value)
		}
	}
	if autoContentType != "" && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", autoContentType)
	}
	if httpReq.Header.Get("User-Agent") == "" {
		httpReq.Header.Set("User-Agent", "InterfaceGo/1.0")
	}

	timeout := time.Duration(req.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: req.TLSSkip}, //nolint:gosec // 自签证书场景由用户显式开启
		TLSHandshakeTimeout: 15 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	if !req.FollowRedirect {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	httpResp, err := client.Do(httpReq)
	if err != nil {
		resp.Error = friendlyErr(err)
		resp.DurationMs = time.Since(start).Milliseconds()
		return resp
	}
	defer httpResp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyBytes+1))
	resp.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		resp.Error = "读取响应失败：" + err.Error()
	}
	truncated := false
	if int64(len(data)) > maxBodyBytes {
		data = data[:maxBodyBytes]
		truncated = true
	}

	resp.Status = httpResp.StatusCode
	resp.StatusText = strings.TrimSpace(strings.TrimPrefix(httpResp.Status, fmt.Sprintf("%d", httpResp.StatusCode)))
	resp.BodyBytes = int64(len(data))
	resp.Truncated = truncated
	resp.ContentType = httpResp.Header.Get("Content-Type")

	for k, vs := range httpResp.Header {
		resp.Headers = append(resp.Headers, model.KV{Key: k, Value: strings.Join(vs, ", "), Enabled: true})
	}
	// 让响应头顺序稳定，便于阅读与比对
	sortKV(resp.Headers)

	resp.Body, resp.Charset = decodeBody(data, resp.ContentType)
	return resp
}

// decodeBody 按字符集解码响应体，兼容 GBK/GB2312/GB18030 老系统。
func decodeBody(data []byte, contentType string) (string, string) {
	if len(data) == 0 {
		return "", "utf-8"
	}
	if !isTextual(contentType, data) {
		return fmt.Sprintf("[二进制内容，共 %d 字节，Content-Type: %s]", len(data), contentType), "binary"
	}
	lower := strings.ToLower(contentType)
	if i := strings.Index(lower, "charset="); i >= 0 {
		cs := lower[i+len("charset="):]
		if j := strings.IndexAny(cs, ";, "); j >= 0 {
			cs = cs[:j]
		}
		cs = strings.Trim(cs, `"'`)
		switch cs {
		case "gbk", "gb2312", "gb18030":
			if s, err := simplifiedchinese.GB18030.NewDecoder().String(string(data)); err == nil {
				return s, cs
			}
		}
	}
	if utf8.Valid(data) {
		return string(data), "utf-8"
	}
	if s, err := simplifiedchinese.GB18030.NewDecoder().String(string(data)); err == nil {
		return s, "gb18030"
	}
	return string(data), ""
}

// isTextual 判断响应是否为文本内容（二进制内容不做字符集解码）。
func isTextual(contentType string, data []byte) bool {
	ct := strings.ToLower(contentType)
	if ct != "" {
		if strings.HasPrefix(ct, "text/") ||
			strings.Contains(ct, "json") ||
			strings.Contains(ct, "xml") ||
			strings.Contains(ct, "javascript") ||
			strings.Contains(ct, "x-www-form-urlencoded") ||
			strings.Contains(ct, "graphql") ||
			strings.Contains(ct, "yaml") {
			return true
		}
		if strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "audio/") ||
			strings.HasPrefix(ct, "video/") || strings.Contains(ct, "octet-stream") ||
			strings.Contains(ct, "pdf") || strings.Contains(ct, "zip") {
			return false
		}
	}
	return utf8.Valid(data)
}

// friendlyErr 把常见网络错误翻译成中文提示。
func friendlyErr(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "Client.Timeout"):
		return "请求超时，可适当增大超时时间"
	case strings.Contains(msg, "connection refused"):
		return "目标拒绝连接，请确认地址与端口是否正确"
	case strings.Contains(msg, "no such host"):
		return "域名解析失败，请确认地址或当前网络能否访问"
	case strings.Contains(msg, "certificate"):
		return "证书校验失败，可勾选「跳过证书校验」后重试"
	case strings.Contains(msg, "context canceled"):
		return "请求已取消"
	}
	return "请求失败：" + msg
}

func sortKV(items []model.KV) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && strings.ToLower(items[j-1].Key) > strings.ToLower(items[j].Key); j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}
