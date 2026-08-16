// Package httpapi 提供负载均衡调度器的 HTTP 接口。
// 服务持有内存状态（节点定义与运行时统计），并发安全。
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"task028-loadbalancer/internal/lb"
)

// ErrBadJSON 表示请求体不是单个合法 JSON 对象。
var ErrBadJSON = errors.New("请求体不是合法的单个 JSON 对象")

// API 是负载均衡服务的 HTTP 接口实现，内含一份调度器存储。
type API struct {
	store *lb.Store
}

// New 创建服务实例，自带空的调度器存储。
func New() *API { return &API{store: lb.New()} }

// Handler 返回 HTTP 路由。
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("POST /nodes", a.upsertNode)
	mux.HandleFunc("POST /nodes/remove", a.removeNode)
	mux.HandleFunc("POST /health", a.setHealth)
	mux.HandleFunc("POST /pick", a.pick)
	mux.HandleFunc("POST /release", a.release)
	mux.HandleFunc("GET /stats", a.stats)
	return mux
}

// decodeJSON 解码单个 JSON 对象，拒绝多段 JSON 与未知字段。
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return ErrBadJSON
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return ErrBadJSON
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// outcome 是写操作端点的统一回应。
type outcome struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// nodeRequest 是注册/更新节点的请求体。weight 接受数字或数字字符串。
type nodeRequest struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Weight  any    `json:"weight"`
}

func (a *API) upsertNode(w http.ResponseWriter, r *http.Request) {
	var req nodeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, outcome{OK: false, Error: err.Error()})
		return
	}
	weight, err := toInt(req.Weight)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, outcome{OK: false, Error: err.Error()})
		return
	}
	if err := a.store.UpsertNode(req.ID, req.Address, weight); err != nil {
		writeJSON(w, http.StatusBadRequest, outcome{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, outcome{OK: true})
}

// idRequest 是按 ID 操作的请求体。
type idRequest struct {
	ID string `json:"id"`
}

func (a *API) removeNode(w http.ResponseWriter, r *http.Request) {
	var req idRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, outcome{OK: false, Error: err.Error()})
		return
	}
	if err := a.store.RemoveNode(req.ID); err != nil {
		writeJSON(w, http.StatusBadRequest, outcome{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, outcome{OK: true})
}

// healthRequest 是设置健康状态的请求体。
type healthRequest struct {
	ID      string `json:"id"`
	Healthy *bool  `json:"healthy"`
}

func (a *API) setHealth(w http.ResponseWriter, r *http.Request) {
	var req healthRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, outcome{OK: false, Error: err.Error()})
		return
	}
	if req.Healthy == nil {
		writeJSON(w, http.StatusBadRequest, outcome{OK: false, Error: "healthy 字段不能为空"})
		return
	}
	if err := a.store.SetHealth(req.ID, *req.Healthy); err != nil {
		writeJSON(w, http.StatusBadRequest, outcome{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, outcome{OK: true})
}

// pickRequest 是选取节点的请求体。
type pickRequest struct {
	Strategy string `json:"strategy"`
}

// pickResponse 是选取节点的回应。
type pickResponse struct {
	OK      bool   `json:"ok"`
	Node    string `json:"node,omitempty"`
	Address string `json:"address,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (a *API) pick(w http.ResponseWriter, r *http.Request) {
	var req pickRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, pickResponse{Error: err.Error()})
		return
	}
	strat, err := lb.ParseStrategy(req.Strategy)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, pickResponse{Error: err.Error()})
		return
	}
	id, addr, err := a.store.Pick(strat)
	if err != nil {
		writeJSON(w, statusFor(err), pickResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, pickResponse{OK: true, Node: id, Address: addr})
}

// releaseRequest 是释放连接的请求体。
type releaseRequest struct {
	Node string `json:"node"`
}

func (a *API) release(w http.ResponseWriter, r *http.Request) {
	var req releaseRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, outcome{OK: false, Error: err.Error()})
		return
	}
	if err := a.store.Release(req.Node); err != nil {
		writeJSON(w, statusFor(err), outcome{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, outcome{OK: true})
}

// statsResponse 是统计查询的回应。
type statsResponse struct {
	Nodes []lb.Stats `json:"nodes"`
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	nodes := a.store.Stats()
	if nodes == nil {
		nodes = []lb.Stats{}
	}
	writeJSON(w, http.StatusOK, statsResponse{Nodes: nodes})
}

// statusFor 将领域错误映射到合适的 HTTP 状态码。
func statusFor(err error) int {
	switch {
	case errors.Is(err, lb.ErrNoAvailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, lb.ErrReleaseUnderflow):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// toInt 接受 JSON 数字或数字字符串，转为 int。
func toInt(v any) (int, error) {
	switch x := v.(type) {
	case float64:
		if x != float64(int(x)) {
			return 0, fmt.Errorf("权重必须是整数")
		}
		return int(x), nil
	case string:
		n, err := strconv.Atoi(x)
		if err != nil {
			return 0, fmt.Errorf("权重不是合法整数: %q", x)
		}
		return n, nil
	case nil:
		return 0, fmt.Errorf("权重不能为空")
	default:
		return 0, fmt.Errorf("权重类型非法")
	}
}
