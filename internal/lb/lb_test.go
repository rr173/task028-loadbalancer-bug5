package lb

import (
	"errors"
	"strings"
	"testing"
)

// mustUpsert 注册若干节点，失败即终止。
func mustUpsert(t *testing.T, s *Store, id, addr string, w int) {
	t.Helper()
	if err := s.UpsertNode(id, addr, w); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

// pickN 用给定策略连续选取 n 次，返回被选节点 ID 序列并占用连接。
func pickN(t *testing.T, s *Store, strat Strategy, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, _, err := s.Pick(strat)
		if err != nil {
			t.Fatalf("pick #%d: %v", i, err)
		}
		out = append(out, id)
	}
	return out
}

// TestWRRSmooth 校验平滑加权轮询的精确序列与反集中性。
func TestWRRSmooth(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 5)
	mustUpsert(t, s, "b", "b:80", 1)
	mustUpsert(t, s, "c", "c:80", 1)

	seq := pickN(t, s, StrategyWRR, 7)
	want := []string{"a", "a", "b", "a", "c", "a", "a"}
	if got := strings.Join(seq, ","); got != strings.Join(want, ",") {
		t.Fatalf("wrr seq=%v want %v", seq, want)
	}
	// 反集中：同一节点连续出现不超过 2 次。
	if maxRun := maxConsecutive(seq); maxRun > 2 {
		t.Errorf("max consecutive run=%d, want <=2", maxRun)
	}
	// 权重比例正确：5 a / 1 b / 1 c。
	if cnt := countOf(seq, "a"); cnt != 5 {
		t.Errorf("a count=%d want 5", cnt)
	}
	if cnt := countOf(seq, "b"); cnt != 1 {
		t.Errorf("b count=%d want 1", cnt)
	}
	if cnt := countOf(seq, "c"); cnt != 1 {
		t.Errorf("c count=%d want 1", cnt)
	}
}

// TestWRRPeriodic 校验一轮权重总和后状态回到起点，序列可复现。
func TestWRRPeriodic(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 2)
	mustUpsert(t, s, "b", "b:80", 1)
	// total weight=3，选取 3 次为一个完整周期。
	first := pickN(t, s, StrategyWRR, 3)
	second := pickN(t, s, StrategyWRR, 3)
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Errorf("wrr not periodic: %v then %v", first, second)
	}
}

// TestRRRoundRobin 校验轮询在注册顺序上循环（全新存储，计数器从 0 起）。
func TestRRRoundRobin(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 1)
	mustUpsert(t, s, "b", "b:80", 1)
	mustUpsert(t, s, "c", "c:80", 1)
	seq := pickN(t, s, StrategyRR, 4)
	want := []string{"a", "b", "c", "a"}
	if got := strings.Join(seq, ","); got != strings.Join(want, ",") {
		t.Fatalf("rr seq=%v want %v", seq, want)
	}
}

// TestRRSkipsIneligible 校验轮询跳过权重为 0 与不健康节点。
func TestRRSkipsIneligible(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 1)
	mustUpsert(t, s, "b", "b:80", 0) // 权重 0，不参与
	mustUpsert(t, s, "c", "c:80", 1)
	mustUpsert(t, s, "d", "d:80", 1)
	if err := s.SetHealth("c", false); err != nil { // 标记 down，不参与
		t.Fatalf("set health: %v", err)
	}
	seq := pickN(t, s, StrategyRR, 4)
	// 可选节点为 [a, d]，轮询序列 a,d,a,d。
	want := []string{"a", "d", "a", "d"}
	if got := strings.Join(seq, ","); got != strings.Join(want, ",") {
		t.Fatalf("rr with ineligible seq=%v want %v", seq, want)
	}
}

// TestLCTieBreak 校验最少连接的平局打破：权重优先、再注册顺序。
func TestLCTieBreak(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 5)
	mustUpsert(t, s, "b", "b:80", 1)
	mustUpsert(t, s, "c", "c:80", 1)
	// 全部活跃 0，首次选权重最高的 a。
	id, _, err := s.Pick(StrategyLC)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if id != "a" {
		t.Errorf("first lc pick=%s want a (highest weight)", id)
	}
	// a 活跃 1，b/c 仍 0；选 b（权重与 c 相同，注册在前）。
	id, _, err = s.Pick(StrategyLC)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if id != "b" {
		t.Errorf("second lc pick=%s want b", id)
	}
	// b 活跃 1，c 仍 0；选 c。
	id, _, err = s.Pick(StrategyLC)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if id != "c" {
		t.Errorf("third lc pick=%s want c", id)
	}
}

// TestLCMinActive 校验最少连接优先选取活跃最少的节点。
func TestLCMinActive(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 1)
	mustUpsert(t, s, "b", "b:80", 1)
	// 让 a 多占一条连接。
	if _, _, err := s.Pick(StrategyWRR); err != nil {
		t.Fatalf("pick a: %v", err)
	}
	// a 活跃 1，b 活跃 0 → LC 应选 b。
	id, _, err := s.Pick(StrategyLC)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if id != "b" {
		t.Errorf("lc pick=%s want b (fewer active)", id)
	}
}

// TestWeightZeroExcluded 校验权重为 0 的节点不参与选取但保留统计。
func TestWeightZeroExcluded(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 0)
	// 仅 a 且权重 0 → 不可选。
	if _, _, err := s.Pick(StrategyRR); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("pick weight-0 only: err=%v want ErrNoAvailable", err)
	}
	// 统计中仍可见 a。
	stats := s.Stats()
	if len(stats) != 1 || stats[0].ID != "a" || stats[0].Weight != 0 {
		t.Errorf("stats=%v want [a w=0]", stats)
	}
}

// TestHealthDownExcluded 校验 down 节点不参与选取。
func TestHealthDownExcluded(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 1)
	if err := s.SetHealth("a", false); err != nil {
		t.Fatalf("set health: %v", err)
	}
	if _, _, err := s.Pick(StrategyWRR); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("pick down only: err=%v want ErrNoAvailable", err)
	}
	// 恢复健康后可选取。
	if err := s.SetHealth("a", true); err != nil {
		t.Fatalf("set health: %v", err)
	}
	id, _, err := s.Pick(StrategyLC)
	if err != nil || id != "a" {
		t.Errorf("after recover: id=%s err=%v want a", id, err)
	}
}

// TestNoAvailable 校验全部不可选时返回明确错误。
func TestNoAvailable(t *testing.T) {
	s := New()
	// 空节点集。
	if _, _, err := s.Pick(StrategyRR); !errors.Is(err, ErrNoAvailable) {
		t.Errorf("empty: err=%v want ErrNoAvailable", err)
	}
	mustUpsert(t, s, "a", "a:80", 1)
	mustUpsert(t, s, "b", "b:80", 1)
	if err := s.SetHealth("a", false); err != nil {
		t.Fatalf("set health: %v", err)
	}
	if err := s.SetHealth("b", false); err != nil {
		t.Fatalf("set health: %v", err)
	}
	for _, strat := range []Strategy{StrategyRR, StrategyWRR, StrategyLC} {
		if _, _, err := s.Pick(strat); !errors.Is(err, ErrNoAvailable) {
			t.Errorf("%s: err=%v want ErrNoAvailable", strat, err)
		}
	}
}

// TestReleaseUnderflow 校验释放不能使活跃连接数为负。
func TestReleaseUnderflow(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 1)
	// 未持有连接即释放。
	if err := s.Release("a"); !errors.Is(err, ErrReleaseUnderflow) {
		t.Errorf("release on zero: err=%v want ErrReleaseUnderflow", err)
	}
	// 占用一条再释放，再释放应失败。
	if _, _, err := s.Pick(StrategyRR); err != nil {
		t.Fatalf("pick: %v", err)
	}
	if err := s.Release("a"); err != nil {
		t.Errorf("release after acquire: %v", err)
	}
	if err := s.Release("a"); !errors.Is(err, ErrReleaseUnderflow) {
		t.Errorf("second release: err=%v want ErrReleaseUnderflow", err)
	}
}

// TestReleaseUnknown 校验对未知节点释放返回错误。
func TestReleaseUnknown(t *testing.T) {
	s := New()
	if err := s.Release("ghost"); !errors.Is(err, ErrUnknownNode) {
		t.Errorf("release unknown: err=%v want ErrUnknownNode", err)
	}
}

// TestUpsertPreservesStats 校验覆盖更新保留活跃连接数与累计选中次数。
func TestUpsertPreservesStats(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 5)
	// 占用 2 条连接。
	if _, _, err := s.Pick(StrategyWRR); err != nil {
		t.Fatalf("pick: %v", err)
	}
	if _, _, err := s.Pick(StrategyWRR); err != nil {
		t.Fatalf("pick: %v", err)
	}
	// 更新权重与地址。
	if err := s.UpsertNode("a", "a:8080", 1); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	stats := s.Stats()
	if len(stats) != 1 {
		t.Fatalf("stats len=%d want 1", len(stats))
	}
	st := stats[0]
	if st.Address != "a:8080" || st.Weight != 1 {
		t.Errorf("update not applied: addr=%s weight=%d", st.Address, st.Weight)
	}
	if st.Active != 2 {
		t.Errorf("active=%d want 2 (preserved)", st.Active)
	}
	if st.Selections != 2 {
		t.Errorf("selections=%d want 2 (preserved)", st.Selections)
	}
}

// TestUpsertValidation 校验注册时的格式校验。
func TestUpsertValidation(t *testing.T) {
	s := New()
	cases := []struct {
		id, addr string
		weight   int
		want     error
	}{
		{"", "a:80", 1, ErrEmptyID},
		{"bad id", "a:80", 1, ErrBadID}, // 含空格
		{"a", "", 1, ErrEmptyAddr},
		{"a", "a:80", -1, ErrNegativeWeight},
	}
	for i, c := range cases {
		err := s.UpsertNode(c.id, c.addr, c.weight)
		if !errors.Is(err, c.want) {
			t.Errorf("case %d: err=%v want %v", i, err, c.want)
		}
	}
	// 全部失败后节点表应仍为空。
	if len(s.Stats()) != 0 {
		t.Errorf("no node should be stored, got %v", s.Stats())
	}
}

// TestRemoveNode 校验移除节点。
func TestRemoveNode(t *testing.T) {
	s := New()
	mustUpsert(t, s, "a", "a:80", 1)
	mustUpsert(t, s, "b", "b:80", 1)
	if err := s.RemoveNode("a"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if s.HasNode("a") {
		t.Error("a should be removed")
	}
	// 移除后选取只命中 b。
	id, _, err := s.Pick(StrategyRR)
	if err != nil || id != "b" {
		t.Errorf("after remove pick=%s err=%v want b", id, err)
	}
	// 重复移除报错。
	if err := s.RemoveNode("a"); !errors.Is(err, ErrUnknownNode) {
		t.Errorf("remove again: err=%v want ErrUnknownNode", err)
	}
}

// TestStatsOrder 校验统计按注册顺序返回。
func TestStatsOrder(t *testing.T) {
	s := New()
	mustUpsert(t, s, "c", "c:80", 1)
	mustUpsert(t, s, "a", "a:80", 1)
	mustUpsert(t, s, "b", "b:80", 1)
	stats := s.Stats()
	got := []string{stats[0].ID, stats[1].ID, stats[2].ID}
	want := []string{"c", "a", "b"} // 注册顺序而非字典序
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stats order=%v want %v", got, want)
	}
}

// TestParseStrategy 校验策略解析。
func TestParseStrategy(t *testing.T) {
	for _, str := range []Strategy{StrategyRR, StrategyWRR, StrategyLC} {
		if got, err := ParseStrategy(string(str)); err != nil || got != str {
			t.Errorf("parse %s: got=%s err=%v", str, got, err)
		}
	}
	if _, err := ParseStrategy("xyz"); err == nil {
		t.Error("parse xyz should fail")
	}
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
