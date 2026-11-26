package ipguard

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var trafficBase = time.Date(2026, 11, 3, 10, 30, 15, 0, time.Local)

func TestTrafficMinutesOutcomes(t *testing.T) {
	tr := NewTraffic(10)
	now := trafficBase
	for _, o := range []string{OutcomeAllowed, OutcomeAllowed, ReasonDenied, ReasonBlacklist, ReasonRateLimit, "weird"} {
		tr.Record(now, "1.2.3.4", "CN", "ua", o)
	}
	got := tr.Minutes(now, 1)
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	c := got[0]
	want := MinuteCount{Minute: now.Truncate(time.Minute), Total: 6, Allowed: 2, Denied: 2, Blacklisted: 1, RateLimited: 1}
	if !c.Minute.Equal(want.Minute) || c.Total != want.Total || c.Allowed != want.Allowed ||
		c.Denied != want.Denied || c.Blacklisted != want.Blacklisted || c.RateLimited != want.RateLimited {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestTrafficMinutesAcrossMinutes(t *testing.T) {
	tr := NewTraffic(10)
	now := trafficBase
	tr.Record(now.Add(-2*time.Minute), "1.2.3.4", "", "", OutcomeAllowed)
	tr.Record(now.Add(-2*time.Minute), "1.2.3.4", "", "", OutcomeAllowed)
	tr.Record(now, "1.2.3.4", "", "", ReasonDenied)

	got := tr.Minutes(now, 3)
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	cur := now.Truncate(time.Minute)
	for i, c := range got {
		if want := cur.Add(time.Duration(i-2) * time.Minute); !c.Minute.Equal(want) {
			t.Fatalf("[%d] minute=%v want %v", i, c.Minute, want)
		}
	}
	if got[0].Total != 2 || got[0].Allowed != 2 || got[1].Total != 0 || got[2].Total != 1 || got[2].Denied != 1 {
		t.Fatalf("got %+v", got)
	}

	// n 截断到 [1,60]
	if l := len(tr.Minutes(now, 0)); l != 1 {
		t.Fatalf("n=0 len=%d", l)
	}
	if l := len(tr.Minutes(now, 1000)); l != 60 {
		t.Fatalf("n=1000 len=%d", l)
	}
}

func TestTrafficMinutesStaleBuckets(t *testing.T) {
	tr := NewTraffic(10)
	old := trafficBase
	tr.Record(old, "1.2.3.4", "", "", OutcomeAllowed)

	// 60 分钟后同一桶位：读取不应看到旧数据
	now := old.Add(60 * time.Minute)
	for _, c := range tr.Minutes(now, 60) {
		if c.Total != 0 {
			t.Fatalf("stale bucket leaked: %+v", c)
		}
	}
	// 写入时旧桶被重置
	tr.Record(now, "1.2.3.4", "", "", ReasonRateLimit)
	ms := tr.Minutes(now, 60)
	last := ms[len(ms)-1]
	if last.Total != 1 || last.Allowed != 0 || last.RateLimited != 1 {
		t.Fatalf("bucket not reset: %+v", last)
	}
	// 59 分钟前的数据仍在窗口内
	tr2 := NewTraffic(10)
	tr2.Record(old, "1.2.3.4", "", "", OutcomeAllowed)
	if ms := tr2.Minutes(old.Add(59*time.Minute), 60); ms[0].Total != 1 {
		t.Fatalf("59min-old bucket lost: %+v", ms[0])
	}
}

func TestTrafficRingOverwrite(t *testing.T) {
	tr := NewTraffic(3)
	for i := 0; i < 5; i++ {
		tr.Record(trafficBase.Add(time.Duration(i)*time.Second), fmt.Sprintf("10.0.%d.1", i), "", "", OutcomeAllowed)
	}
	ra := tr.Recent(0)
	if ra.Total != 3 || ra.Capacity != 3 || ra.Subnets != 3 {
		t.Fatalf("got %+v", ra)
	}
	if want := trafficBase.Add(2 * time.Second); !ra.Oldest.Equal(want) {
		t.Fatalf("oldest=%v want %v", ra.Oldest, want)
	}
	for _, g := range ra.Groups {
		if g.Subnet == "10.0.0.0/24" || g.Subnet == "10.0.1.0/24" {
			t.Fatalf("overwritten entry still present: %+v", g)
		}
	}
	if NewTraffic(0).Recent(0).Capacity != DefaultRecentCapacity {
		t.Fatal("default capacity")
	}
}

func TestTrafficRecentGroups(t *testing.T) {
	tr := NewTraffic(100)
	ts := trafficBase
	rec := func(ip, country, ua, outcome string) {
		ts = ts.Add(time.Second)
		tr.Record(ts, ip, country, ua, outcome)
	}
	// 1.2.3.0/24：4 条，2 个 IP，3 种 UA，国家多数 US，2 条拦截
	rec("1.2.3.4", "US", "bot", OutcomeAllowed)
	rec("1.2.3.4", "US", "bot", ReasonDenied)
	rec("1.2.3.9", "CN", "", ReasonBlacklist)
	rec("1.2.3.9", "", "curl", OutcomeAllowed)
	last13 := ts
	// 5.6.7.0/24 与 9.9.9.0/24 各 2 条（同数按 Subnet 升序）
	rec("9.9.9.9", "", "x", OutcomeAllowed)
	rec("9.9.9.8", "", "x", OutcomeAllowed)
	rec("5.6.7.8", "", "y", ReasonRateLimit)
	rec("5.6.7.8", "", "y", OutcomeAllowed)
	// 1 条 IPv6
	rec("2001:db8:1:2::1", "JP", "z", OutcomeAllowed)
	// 非法 IP：计入 Total，不分组
	rec("not-an-ip", "", "", OutcomeAllowed)

	ra := tr.Recent(0)
	if ra.Total != 10 || ra.Subnets != 4 || len(ra.Groups) != 4 {
		t.Fatalf("total=%d subnets=%d groups=%d", ra.Total, ra.Subnets, len(ra.Groups))
	}
	order := []string{"1.2.3.0/24", "5.6.7.0/24", "9.9.9.0/24", "2001:db8:1::/48"}
	for i, s := range order {
		if ra.Groups[i].Subnet != s {
			t.Fatalf("order[%d]=%s want %s", i, ra.Groups[i].Subnet, s)
		}
	}
	g := ra.Groups[0]
	if g.Count != 4 || g.Blocked != 2 || g.IPs != 2 || g.UAs != 3 || g.Country != "US" ||
		g.TopUA != "bot" || !g.LastSeen.Equal(last13) || g.Blacklisted {
		t.Fatalf("group0 %+v", g)
	}
	if g := ra.Groups[1]; g.Blocked != 1 || g.IPs != 1 || g.Country != "" {
		t.Fatalf("group1 %+v", g)
	}
	if g := ra.Groups[2]; g.IPs != 2 || g.UAs != 1 || g.Blocked != 0 {
		t.Fatalf("group2 %+v", g)
	}

	lim := tr.Recent(2)
	if len(lim.Groups) != 2 || lim.Subnets != 4 || lim.Groups[0].Subnet != "1.2.3.0/24" {
		t.Fatalf("limit %+v", lim)
	}

	empty := NewTraffic(5).Recent(10)
	if empty.Groups == nil || empty.Total != 0 || !empty.Oldest.IsZero() {
		t.Fatalf("empty %+v", empty)
	}
}

func TestTrafficUATruncate(t *testing.T) {
	tr := NewTraffic(2)
	tr.Record(trafficBase, "1.2.3.4", "", strings.Repeat("a", 500), OutcomeAllowed)
	if ua := tr.Recent(0).Groups[0].TopUA; len(ua) != 300 {
		t.Fatalf("ua len=%d", len(ua))
	}
}

func TestSubnetOf(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":              "1.2.3.0/24",
		"::ffff:1.2.3.4":       "1.2.3.0/24",
		"2001:db8:abcd:12::1":  "2001:db8:abcd::/48",
		"2001:db8:abcd:ffff::": "2001:db8:abcd::/48",
		"fe80::1%eth0":         "fe80::/48",
		"":                     "",
		"1.2.3":                "",
		"bogus":                "",
	}
	for in, want := range cases {
		if got := SubnetOf(in); got != want {
			t.Errorf("SubnetOf(%q)=%q want %q", in, got, want)
		}
	}
}

func TestTrafficReset(t *testing.T) {
	tr := NewTraffic(5)
	tr.Record(trafficBase, "1.2.3.4", "US", "ua", OutcomeAllowed)
	tr.Reset()
	if ra := tr.Recent(0); ra.Total != 0 || len(ra.Groups) != 0 || ra.Capacity != 5 {
		t.Fatalf("recent after reset %+v", ra)
	}
	if c := tr.Minutes(trafficBase, 1)[0]; c.Total != 0 {
		t.Fatalf("minutes after reset %+v", c)
	}
	tr.Record(trafficBase, "1.2.3.4", "US", "ua", OutcomeAllowed)
	if ra := tr.Recent(0); ra.Total != 1 {
		t.Fatalf("record after reset %+v", ra)
	}
}

func TestTrafficConcurrent(t *testing.T) {
	tr := NewTraffic(50)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				now := time.Now()
				tr.Record(now, fmt.Sprintf("10.%d.0.%d", w, i%250), "CN", "ua", OutcomeAllowed)
				if i%20 == 0 {
					tr.Recent(5)
					tr.Minutes(now, 60)
				}
				if w == 0 && i == 100 {
					tr.Reset()
				}
			}
		}(w)
	}
	wg.Wait()
	if ra := tr.Recent(0); ra.Total != 50 {
		t.Fatalf("total=%d", ra.Total)
	}
}
