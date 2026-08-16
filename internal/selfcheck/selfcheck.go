// Package selfcheck 提供无需外部依赖的自检：每个检查用例在独立的 httptest
// 服务（独立存储）上运行，避免全局 pick 的跨用例污染。覆盖加权轮询平滑性、
// 轮询均衡、最少连接选取与平局打破、健康下线排除、权重为零排除、全部不可选
// 报错、释放非负约束、覆盖更新保留统计等路径。成功返回 0，任一失败返回 1。
package selfcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"task028-loadbalancer/internal/httpapi"
)

// client 封装一个独立的服务实例（独立存储）及其端点调用。
type client struct {
	srv *httptest.Server
}

func newClient() *client {
	return &client{srv: httptest.NewServer(httpapi.New().Handler())}
}

func (c *client) close() { c.srv.Close() }

func (c *client) do(method, path, body string) (*http.Response, []byte, error) {
	var r io.Reader
	if body != "" {
		r = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, c.srv.URL+path, r)
	if err != nil {
		return nil, nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	data, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data, readErr
}

func marshal(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func (c *client) upsert(id, addr string, weight int) (int, bool, string, error) {
	resp, data, err := c.do(http.MethodPost, "/nodes", marshal(map[string]any{"id": id, "address": addr, "weight": weight}))
	if err != nil {
		return 0, false, "", err
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out.OK, out.Error, nil
}

func (c *client) removeNode(id string) (int, bool, string, error) {
	resp, data, err := c.do(http.MethodPost, "/nodes/remove", marshal(map[string]any{"id": id}))
	if err != nil {
		return 0, false, "", err
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out.OK, out.Error, nil
}

func (c *client) setHealth(id string, healthy bool) (int, bool, string, error) {
	resp, data, err := c.do(http.MethodPost, "/health", marshal(map[string]any{"id": id, "healthy": healthy}))
	if err != nil {
		return 0, false, "", err
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out.OK, out.Error, nil
}

func (c *client) pick(strategy string) (int, bool, string, string, error) {
	resp, data, err := c.do(http.MethodPost, "/pick", marshal(map[string]any{"strategy": strategy}))
	if err != nil {
		return 0, false, "", "", err
	}
	var out struct {
		OK      bool   `json:"ok"`
		Node    string `json:"node"`
		Address string `json:"address"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out.OK, out.Node, out.Error, nil
}

func (c *client) release(node string) (int, bool, string, error) {
	resp, data, err := c.do(http.MethodPost, "/release", marshal(map[string]any{"node": node}))
	if err != nil {
		return 0, false, "", err
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out.OK, out.Error, nil
}

func (c *client) stats() (int, []map[string]any, error) {
	resp, data, err := c.do(http.MethodGet, "/stats", "")
	if err != nil {
		return 0, nil, err
	}
	var out struct {
		Nodes []map[string]any `json:"nodes"`
	}
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out.Nodes, nil
}

func statByID(nodes []map[string]any, id string) map[string]any {
	for _, n := range nodes {
		if n["id"] == id {
			return n
		}
	}
	return nil
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	}
	return -1
}

func countOf(seq []string, id string) int {
	n := 0
	for _, x := range seq {
		if x == id {
			n++
		}
	}
	return n
}

func maxConsecutive(seq []string) int {
	max, run := 0, 0
	var prev string
	for _, x := range seq {
		if x == prev {
			run++
		} else {
			run = 1
		}
		if run > max {
			max = run
		}
		prev = x
	}
	return max
}

// Run 执行自检并返回退出码。
func Run() int {
	passed, failed := 0, 0
	check := func(name string, fn func(c *client) error) {
		c := newClient()
		defer c.close()
		if err := fn(c); err != nil {
			failed++
			fmt.Printf("FAIL %-36s %v\n", name, err)
		} else {
			passed++
			fmt.Printf("PASS %s\n", name)
		}
	}

	// ---- 健康检查 ----
	check("健康检查", func(c *client) error {
		resp, _, err := c.do(http.MethodGet, "/healthz", "")
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status=%d", resp.StatusCode)
		}
		return nil
	})

	// ---- 加权轮询平滑性 ----
	check("加权轮询平滑序列", func(c *client) error {
		for _, n := range []struct {
			id string
			w  int
		}{{"a", 5}, {"b", 1}, {"c", 1}} {
			if status, ok, _, err := c.upsert(n.id, n.id+":80", n.w); err != nil || status != http.StatusOK || !ok {
				return fmt.Errorf("upsert %s: status=%d ok=%v err=%v", n.id, status, ok, err)
			}
		}
		seq := make([]string, 0, 7)
		for i := 0; i < 7; i++ {
			status, ok, node, errStr, err := c.pick("wrr")
			if err != nil {
				return err
			}
			if status != http.StatusOK || !ok {
				return fmt.Errorf("wrr pick #%d: status=%d ok=%v err=%s", i, status, ok, errStr)
			}
			seq = append(seq, node)
		}
		want := []string{"a", "a", "b", "a", "c", "a", "a"}
		if got := strings.Join(seq, ","); got != strings.Join(want, ",") {
			return fmt.Errorf("wrr seq=%v want %v", seq, want)
		}
		if maxRun := maxConsecutive(seq); maxRun > 2 {
			return fmt.Errorf("max consecutive run=%d want <=2", maxRun)
		}
		if got := countOf(seq, "a"); got != 5 {
			return fmt.Errorf("a count=%d want 5", got)
		}
		return nil
	})

	// ---- 轮询均衡 ----
	check("轮询覆盖全部可选节点", func(c *client) error {
		for _, n := range []struct {
			id string
			w  int
		}{{"a", 1}, {"b", 1}, {"c", 1}} {
			if status, ok, _, err := c.upsert(n.id, n.id+":80", n.w); err != nil || status != http.StatusOK || !ok {
				return fmt.Errorf("upsert %s: err=%v", n.id, err)
			}
		}
		seen := map[string]bool{}
		for i := 0; i < 3; i++ {
			status, ok, node, errStr, err := c.pick("rr")
			if err != nil {
				return err
			}
			if status != http.StatusOK || !ok {
				return fmt.Errorf("rr pick #%d: status=%d ok=%v err=%s", i, status, ok, errStr)
			}
			seen[node] = true
		}
		if len(seen) != 3 {
			return fmt.Errorf("rr covered %d nodes, want 3: %v", len(seen), seen)
		}
		// 再选 3 次仍覆盖全部（周期性）。
		seen2 := map[string]bool{}
		for i := 0; i < 3; i++ {
			_, ok, node, _, err := c.pick("rr")
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("rr second round pick failed")
			}
			seen2[node] = true
		}
		if len(seen2) != 3 {
			return fmt.Errorf("rr second round covered %d, want 3", len(seen2))
		}
		return nil
	})

	// ---- 最少连接：平局打破 ----
	check("最少连接平局按权重打破", func(c *client) error {
		for _, n := range []struct {
			id string
			w  int
		}{{"a", 5}, {"b", 1}, {"c", 1}} {
			if status, ok, _, err := c.upsert(n.id, n.id+":80", n.w); err != nil || status != http.StatusOK || !ok {
				return fmt.Errorf("upsert %s: err=%v", n.id, err)
			}
		}
		// 全部活跃 0，首次选权重最高者。
		status, ok, node, errStr, err := c.pick("lc")
		if err != nil {
			return err
		}
		if status != http.StatusOK || !ok || node != "a" {
			return fmt.Errorf("first lc pick=%s status=%d ok=%v err=%s want a", node, status, ok, errStr)
		}
		// 第二次：a 活跃 1，b/c 活跃 0，权重相同取注册在前者 b。
		status, ok, node, errStr, err = c.pick("lc")
		if err != nil {
			return err
		}
		if status != http.StatusOK || !ok || node != "b" {
			return fmt.Errorf("second lc pick=%s status=%d ok=%v err=%s want b", node, status, ok, errStr)
		}
		// 第三次：c 仍活跃 0。
		status, ok, node, errStr, err = c.pick("lc")
		if err != nil {
			return err
		}
		if status != http.StatusOK || !ok || node != "c" {
			return fmt.Errorf("third lc pick=%s status=%d ok=%v err=%s want c", node, status, ok, errStr)
		}
		// 三次后各节点活跃连接数应均衡（各 1）。
		_, nodes, err := c.stats()
		if err != nil {
			return err
		}
		for _, id := range []string{"a", "b", "c"} {
			st := statByID(nodes, id)
			if st == nil {
				return fmt.Errorf("missing stat %s", id)
			}
			if act := toInt64(st["active"]); act != 1 {
				return fmt.Errorf("after 3 lc picks %s active=%d want 1", id, act)
			}
		}
		return nil
	})

	// ---- 最少连接：优先活跃最少 ----
	check("最少连接优先活跃最少", func(c *client) error {
		if status, ok, _, err := c.upsert("a", "a:80", 1); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("upsert a: err=%v", err)
		}
		if status, ok, _, err := c.upsert("b", "b:80", 1); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("upsert b: err=%v", err)
		}
		// 权重相同、活跃均 0，首次 LC 命中注册在前者 a，使其活跃 +1。
		status, ok, node, errStr, err := c.pick("lc")
		if err != nil {
			return err
		}
		if status != http.StatusOK || !ok || node != "a" {
			return fmt.Errorf("first lc pick=%s status=%d ok=%v err=%s want a", node, status, ok, errStr)
		}
		// 再选 LC，b 活跃 0 < a 活跃 1，应选 b。
		status, ok, node, errStr, err = c.pick("lc")
		if err != nil {
			return err
		}
		if status != http.StatusOK || !ok || node != "b" {
			return fmt.Errorf("lc pick=%s status=%d ok=%v err=%s want b", node, status, ok, errStr)
		}
		return nil
	})

	// ---- 健康下线排除 ----
	check("健康下线节点被排除", func(c *client) error {
		for _, n := range []struct {
			id string
			w  int
		}{{"a", 1}, {"b", 1}} {
			if status, ok, _, err := c.upsert(n.id, n.id+":80", n.w); err != nil || !ok || status != http.StatusOK {
				return fmt.Errorf("upsert %s: err=%v", n.id, err)
			}
		}
		if status, ok, errStr, err := c.setHealth("a", false); err != nil || status != http.StatusOK || !ok {
			return fmt.Errorf("set a down: status=%d ok=%v err=%s %v", status, ok, errStr, err)
		}
		// 连续选 4 次只应命中 b。
		for i := 0; i < 4; i++ {
			status, ok, node, _, err := c.pick("rr")
			if err != nil {
				return err
			}
			if status != http.StatusOK || !ok || node != "b" {
				return fmt.Errorf("after a down pick #%d=%s want b", i, node)
			}
		}
		// 统计中 a 仍可见且标记为 down。
		_, nodes, err := c.stats()
		if err != nil {
			return err
		}
		st := statByID(nodes, "a")
		if st == nil {
			return fmt.Errorf("a should still be in stats")
		}
		if st["healthy"] != false {
			return fmt.Errorf("a healthy=%v want false", st["healthy"])
		}
		return nil
	})

	// ---- 权重为零排除但保留统计 ----
	check("权重为零节点被排除但保留", func(c *client) error {
		if status, ok, _, err := c.upsert("a", "a:80", 0); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("upsert a w=0: err=%v", err)
		}
		status, ok, _, errStr, err := c.pick("wrr")
		if err != nil {
			return err
		}
		if status != http.StatusServiceUnavailable || ok {
			return fmt.Errorf("pick w=0 only: status=%d ok=%v err=%s want 503/false", status, ok, errStr)
		}
		// 统计中仍可见。
		_, nodes, err := c.stats()
		if err != nil {
			return err
		}
		if statByID(nodes, "a") == nil {
			return fmt.Errorf("a should remain in stats")
		}
		return nil
	})

	// ---- 全部不可选报错 ----
	check("全部不可选返回 503", func(c *client) error {
		if status, ok, _, err := c.upsert("a", "a:80", 1); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("upsert a: err=%v", err)
		}
		if status, ok, _, err := c.upsert("b", "b:80", 1); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("upsert b: err=%v", err)
		}
		if _, _, _, err := c.setHealth("a", false); err != nil {
			return err
		}
		if _, _, _, err := c.setHealth("b", false); err != nil {
			return err
		}
		for _, strat := range []string{"rr", "wrr", "lc"} {
			status, ok, node, errStr, err := c.pick(strat)
			if err != nil {
				return err
			}
			if status != http.StatusServiceUnavailable || ok || node != "" {
				return fmt.Errorf("pick %s on all-down: status=%d ok=%v node=%q err=%s want 503/false/empty", strat, status, ok, node, errStr)
			}
			if !strings.Contains(errStr, "没有可用") {
				return fmt.Errorf("pick %s error should mention 没有可用, got: %q", strat, errStr)
			}
		}
		return nil
	})

	// ---- 释放非负约束 ----
	check("释放非负约束", func(c *client) error {
		if status, ok, _, err := c.upsert("a", "a:80", 1); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("upsert a: err=%v", err)
		}
		// 活跃 0 时释放。
		status, ok, errStr, err := c.release("a")
		if err != nil {
			return err
		}
		if status != http.StatusConflict || ok {
			return fmt.Errorf("release on zero: status=%d ok=%v err=%s want 409/false", status, ok, errStr)
		}
		if !strings.Contains(errStr, "活跃连接数已为 0") {
			return fmt.Errorf("error should mention 活跃连接数已为 0, got: %q", errStr)
		}
		// 占用一条再释放成功，再释放失败。
		if _, ok, _, _, err := c.pick("rr"); err != nil || !ok {
			return fmt.Errorf("pick: err=%v ok=%v", err, ok)
		}
		if status, ok, errStr, err := c.release("a"); err != nil || status != http.StatusOK || !ok {
			return fmt.Errorf("release after acquire: status=%d ok=%v err=%s %v", status, ok, errStr, err)
		}
		if status, ok, errStr, err := c.release("a"); err != nil {
			return err
		} else if status != http.StatusConflict || ok {
			return fmt.Errorf("second release: status=%d ok=%v err=%s want 409/false", status, ok, errStr)
		}
		return nil
	})

	// ---- 覆盖更新保留统计 ----
	check("覆盖更新保留统计", func(c *client) error {
		if status, ok, _, err := c.upsert("a", "a:80", 5); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("upsert a: err=%v", err)
		}
		// 占用 2 条连接（单节点，均命中 a）。
		for i := 0; i < 2; i++ {
			if status, ok, _, errStr, err := c.pick("wrr"); err != nil || status != http.StatusOK || !ok {
				return fmt.Errorf("pick #%d: status=%d ok=%v err=%s %v", i, status, ok, errStr, err)
			}
		}
		// 更新权重与地址。
		if status, ok, _, err := c.upsert("a", "a:8080", 1); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("upsert a update: err=%v", err)
		}
		_, nodes, err := c.stats()
		if err != nil {
			return err
		}
		st := statByID(nodes, "a")
		if st == nil {
			return fmt.Errorf("a missing from stats")
		}
		if st["address"] != "a:8080" || toInt64(st["weight"]) != 1 {
			return fmt.Errorf("update not applied: addr=%v weight=%v", st["address"], st["weight"])
		}
		if toInt64(st["active"]) != 2 {
			return fmt.Errorf("active=%v want 2 (preserved)", st["active"])
		}
		if toInt64(st["selections"]) != 2 {
			return fmt.Errorf("selections=%v want 2 (preserved)", st["selections"])
		}
		return nil
	})

	// ---- 注册校验 ----
	check("注册校验拒绝非法输入", func(c *client) error {
		cases := []struct {
			body string
			desc string
		}{
			{marshal(map[string]any{"id": "", "address": "v:80", "weight": 1}), "空 ID"},
			{marshal(map[string]any{"id": "v-a", "address": "", "weight": 1}), "空地址"},
			{marshal(map[string]any{"id": "v-a", "address": "v:80", "weight": -1}), "负权重"},
		}
		for _, cse := range cases {
			resp, _, err := c.do(http.MethodPost, "/nodes", cse.body)
			if err != nil {
				return fmt.Errorf("%s: %v", cse.desc, err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				return fmt.Errorf("%s: status=%d want 400", cse.desc, resp.StatusCode)
			}
		}
		// 非法 JSON。
		resp, _, err := c.do(http.MethodPost, "/nodes", "{not json")
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("bad json: status=%d want 400", resp.StatusCode)
		}
		// 未知节点操作。
		if status, ok, _, err := c.removeNode("ghost"); err != nil || status != http.StatusBadRequest || ok {
			return fmt.Errorf("remove unknown: status=%d ok=%v err=%v want 400/false", status, ok, err)
		}
		// 未知策略。
		resp, _, err = c.do(http.MethodPost, "/pick", marshal(map[string]any{"strategy": "xyz"}))
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("bad strategy: status=%d want 400", resp.StatusCode)
		}
		return nil
	})

	// ---- 移除节点 ----
	check("移除节点后不再被选中", func(c *client) error {
		if status, ok, _, err := c.upsert("a", "a:80", 1); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("upsert a: err=%v", err)
		}
		if status, ok, _, err := c.upsert("b", "b:80", 1); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("upsert b: err=%v", err)
		}
		if status, ok, _, err := c.removeNode("a"); err != nil || !ok || status != http.StatusOK {
			return fmt.Errorf("remove a: err=%v", err)
		}
		// 连续选 3 次只命中 b。
		for i := 0; i < 3; i++ {
			status, ok, node, _, err := c.pick("rr")
			if err != nil {
				return err
			}
			if status != http.StatusOK || !ok || node != "b" {
				return fmt.Errorf("after remove pick #%d=%s want b", i, node)
			}
		}
		return nil
	})

	fmt.Printf("\n%d passed, %d failed\n", passed, failed)
	if failed > 0 {
		return 1
	}
	return 0
}
