// Package model 定义接口工具的全部数据结构。
package model

// 支持的请求方法（HTTP 全部常用方法）。
var Methods = []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "TRACE"}

// Body 模式。
const (
	BodyNone       = "none"
	BodyJSON       = "json"
	BodyRaw        = "raw"
	BodyForm       = "form"       // multipart/form-data
	BodyURLEncoded = "urlencoded" // application/x-www-form-urlencoded
)

// KV 是可编辑的参数行，用于 query / header / 表单 / 路径参数。
type KV struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Desc    string `json:"desc,omitempty"`
	Enabled bool   `json:"enabled"`
}

// Request 是一次请求的完整配置。
type Request struct {
	Method         string `json:"method"`
	URL            string `json:"url"`
	PathVars       []KV   `json:"pathVars,omitempty"`
	Query          []KV   `json:"query,omitempty"`
	Headers        []KV   `json:"headers,omitempty"`
	BodyMode       string `json:"bodyMode"`
	Body           string `json:"body,omitempty"`
	FormData       []KV   `json:"formData,omitempty"`
	ContentType    string `json:"contentType,omitempty"`
	Timeout        int    `json:"timeout"`
	TLSSkip        bool   `json:"tlsSkip"`
	FollowRedirect bool   `json:"followRedirect"`
}

// Response 是一次请求的响应快照。
type Response struct {
	Status      int    `json:"status"`
	StatusText  string `json:"statusText"`
	Headers     []KV   `json:"headers"`
	Body        string `json:"body"`
	BodyBytes   int64  `json:"bodyBytes"`
	DurationMs  int64  `json:"durationMs"`
	Truncated   bool   `json:"truncated"`
	ContentType string `json:"contentType,omitempty"`
	Charset     string `json:"charset,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Snapshot 是接口副本：冻结的请求配置 + 保存时的出参。
type Snapshot struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Note      string   `json:"note,omitempty"`
	Doc       string   `json:"doc,omitempty"`
	CreatedAt int64    `json:"createdAt"`
	Request   Request  `json:"request"`
	Response  Response `json:"response"`
}

// Interface 是一个接口配置。
type Interface struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Group     string     `json:"group"`
	Request   Request    `json:"request"`
	Response  Response   `json:"response"`
	Doc       string     `json:"doc"`
	CreatedAt int64      `json:"createdAt"`
	UpdatedAt int64      `json:"updatedAt"`
	Snapshots []Snapshot `json:"snapshots"`
}

// Workspace 是整个数据文件的根。
type Workspace struct {
	Version    int          `json:"version"`
	Interfaces []*Interface `json:"interfaces"`
}

// NewRequest 返回带默认值的请求配置。
func NewRequest() Request {
	return Request{
		Method:         "GET",
		URL:            "",
		PathVars:       []KV{},
		Query:          []KV{},
		Headers:        []KV{},
		BodyMode:       BodyNone,
		Body:           "",
		FormData:       []KV{},
		Timeout:        30,
		TLSSkip:        false,
		FollowRedirect: true,
	}
}

// Normalize 补齐历史数据的缺失字段，保证前端拿到的结构完整。
func (r *Request) Normalize() {
	if r.Method == "" {
		r.Method = "GET"
	}
	if r.BodyMode == "" {
		r.BodyMode = BodyNone
	}
	if r.Timeout <= 0 {
		r.Timeout = 30
	}
	if r.PathVars == nil {
		r.PathVars = []KV{}
	}
	if r.Query == nil {
		r.Query = []KV{}
	}
	if r.Headers == nil {
		r.Headers = []KV{}
	}
	if r.FormData == nil {
		r.FormData = []KV{}
	}
}

// Normalize 补齐响应缺失字段。
func (r *Response) Normalize() {
	if r.Headers == nil {
		r.Headers = []KV{}
	}
}

// Normalize 补齐接口缺失字段。
func (i *Interface) Normalize() {
	i.Request.Normalize()
	i.Response.Normalize()
	if i.Snapshots == nil {
		i.Snapshots = []Snapshot{}
	}
	for idx := range i.Snapshots {
		i.Snapshots[idx].Request.Normalize()
		i.Snapshots[idx].Response.Normalize()
	}
}
