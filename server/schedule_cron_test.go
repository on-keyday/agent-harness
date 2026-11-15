package server

import (
	"fmt"
	"math/bits"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestParseCronRefuses(t *testing.T) {
	for _, expr := range []string{
		"TZ=", "CRON_TZ=UTC 0 0 * * *", " TZ=Asia/Tokyo 0 9 * * *", "@every 1h", "@reboot",
		"0 0 30 2 *", "60 * * * *", "* * * * * *", "5-1 * * * *", "*/0 * * * *",
		"1,,2 * * * *", "0 0 ? * *", "0 0 L * *", "", "   ", strings.Repeat("1,", 128) + "1 * * * *",
		"0 24 * * *", "0 0 0 * *", "0 0 * 13 *", "0 0 * * 8", "0 0 * * *-", "0 0 * * mon-fr",
		"1-2-3 * * * *", "1/2/3 * * * *", "*-5 * * * *", "-1 * * * *", "+1 * * * *",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("parseCron(%q) panicked: %v", expr, r)
				}
			}()
			if c, err := parseCron(expr); err == nil {
				t.Errorf("parseCron(%q) = %+v, want error", expr, c)
			}
		}()
	}
}

func TestParseCronAccepts(t *testing.T) {
	for _, expr := range []string{
		"0 9 * * 1-5", "*/30 * * * *", "@daily", "@DAILY", "@Midnight", "@hourly", "@weekly", "@monthly",
		"@yearly", "@annually", "0 9 13 * 5", "0 9 * * 7", "0 0 * jan,JUL mon-fri", "5/15 * * * *",
	} {
		if _, err := parseCron(expr); err != nil {
			t.Errorf("parseCron(%q): %v", expr, err)
		}
	}
}

func wall(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}

func mustCron(t *testing.T, expr string) cronExpr {
	t.Helper()
	c, err := parseCron(expr)
	if err != nil {
		t.Fatalf("parseCron(%q): %v", expr, err)
	}
	return c
}

func TestNextWallTable(t *testing.T) {
	// 2026-11-15 is a Sunday.
	seq := func(expr string, from time.Time, n int) []time.Time {
		c := mustCron(t, expr)
		var out []time.Time
		for range n {
			from = c.nextWall(from)
			out = append(out, from)
		}
		return out
	}
	check := func(name string, got []time.Time, want ...time.Time) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: len", name)
		}
		for i := range want {
			if !got[i].Equal(want[i]) {
				t.Errorf("%s[%d] = %v, want %v", name, i, got[i], want[i])
			}
		}
	}
	start := wall(2026, 11, 15, 8, 59)
	check("or rule", seq("0 9 13 * 5", start, 1), wall(2026, 11, 20, 9, 0))
	check("dow 7", seq("0 9 * * 7", start, 2), wall(2026, 11, 15, 9, 0), wall(2026, 11, 22, 9, 0))
	check("dow 0", seq("0 9 * * 0", start, 2), wall(2026, 11, 15, 9, 0), wall(2026, 11, 22, 9, 0))
	// dom starts with * -> AND: odd days that are Mondays. 2026-11-16 is a Monday (even, no);
	// 2026-11-23 is the first odd one.
	check("and rule", seq("0 0 */2 * 1", start, 1), wall(2026, 11, 23, 0, 0))
	check("step N/S", seq("5/15 * * * *", wall(2026, 11, 15, 10, 0), 5),
		wall(2026, 11, 15, 10, 5), wall(2026, 11, 15, 10, 20), wall(2026, 11, 15, 10, 35),
		wall(2026, 11, 15, 10, 50), wall(2026, 11, 15, 11, 5))
	check("leap", seq("0 0 29 2 *", wall(2026, 3, 1, 0, 0), 1), wall(2028, 2, 29, 0, 0))
	// strictly after, even with seconds
	c := mustCron(t, "* * * * *")
	if got := c.nextWall(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)); !got.Equal(wall(2026, 1, 1, 0, 1)) {
		t.Errorf("seconds: %v", got)
	}
	if got := c.nextWall(wall(2026, 1, 1, 0, 0)); !got.Equal(wall(2026, 1, 1, 0, 1)) {
		t.Errorf("exact minute: %v", got)
	}
}

func TestNextFiringOrRule(t *testing.T) {
	c := mustCron(t, "0 9 13 * 5")
	jst, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	got := nextFiring(c, jst, time.Date(2026, 11, 15, 8, 59, 0, 0, jst))
	want := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("nextFiring = %v, want %v (UTC)", got, want)
	}
}

func bruteMatch(c cronExpr, t time.Time) bool {
	if c.minute>>uint(t.Minute())&1 == 0 || c.hour>>uint(t.Hour())&1 == 0 || c.month>>uint(t.Month())&1 == 0 {
		return false
	}
	dom := c.dom>>uint(t.Day())&1 == 1
	dow := c.dow>>uint(t.Weekday())&1 == 1
	if c.domStar || c.dowStar {
		return dom && dow
	}
	return dom || dow
}

func randField(r *rand.Rand, lo, hi int) string {
	item := func() string {
		a := lo + r.Intn(hi-lo+1)
		b := a + r.Intn(hi-a+1)
		s := 1 + r.Intn(7)
		switch r.Intn(6) {
		case 0:
			return "*"
		case 1:
			return fmt.Sprintf("*/%d", s)
		case 2:
			return fmt.Sprint(a)
		case 3:
			return fmt.Sprintf("%d-%d", a, b)
		case 4:
			return fmt.Sprintf("%d-%d/%d", a, b, s)
		default:
			return fmt.Sprintf("%d/%d", a, s)
		}
	}
	s := item()
	for r.Intn(3) == 0 {
		s += "," + item()
	}
	return s
}

func TestNextWallMatchesBruteForce(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	checked := 0
	for checked < 500 {
		expr := strings.Join([]string{
			randField(r, 0, 59), randField(r, 0, 23), randField(r, 1, 31), randField(r, 1, 12), randField(r, 0, 7),
		}, " ")
		c, err := parseCron(expr)
		if err != nil {
			continue // e.g. never fires within 5 years
		}
		after := time.Date(2026+r.Intn(5), time.Month(1+r.Intn(12)), 1+r.Intn(28), r.Intn(24), r.Intn(60), r.Intn(60), 0, time.UTC)
		limit := after.AddDate(0, 0, 400)
		var want time.Time
		for m := after.Truncate(time.Minute).Add(time.Minute); m.Before(limit); m = m.Add(time.Minute) {
			if bruteMatch(c, m) {
				want = m
				break
			}
		}
		got := c.nextWall(after)
		if want.IsZero() {
			if !got.IsZero() && got.Before(limit) {
				t.Fatalf("%q after %v: got %v, brute force found none before %v", expr, after, got, limit)
			}
			continue
		}
		checked++
		if !got.Equal(want) {
			t.Fatalf("%q after %v: got %v, want %v", expr, after, got, want)
		}
	}
}

func loadTestZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	return loc
}

func utc(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}

// walkOnePerDay fires `expr` from start for 5 calendar days and checks each fires exactly once.
func walkOnePerDay(t *testing.T, c cronExpr, loc *time.Location, start time.Time) {
	t.Helper()
	perDay := map[string]int{}
	at := start
	end := start.AddDate(0, 0, 4).Add(24 * time.Hour)
	for {
		n := nextFiring(c, loc, at)
		if n.IsZero() || !n.Before(end) {
			break
		}
		perDay[n.In(loc).Format("2006-01-02")]++
		at = n
	}
	for d := 0; d <= 4; d++ {
		day := start.AddDate(0, 0, d).Format("2006-01-02")
		if perDay[day] != 1 {
			t.Errorf("%s from %s: %s fired %d times, want 1 (%v)", loc, start.Format("2006-01-02"), day, perDay[day], perDay)
		}
	}
}

func TestNextAcrossDST(t *testing.T) {
	ny := loadTestZone(t, "America/New_York")
	berlin := loadTestZone(t, "Europe/Berlin")
	sydney := loadTestZone(t, "Australia/Sydney")
	c := mustCron(t, "30 2 * * *")
	for _, tc := range []struct {
		loc   *time.Location
		start time.Time
	}{
		{ny, time.Date(2027, 3, 12, 0, 0, 0, 0, ny)},
		{ny, time.Date(2027, 11, 5, 0, 0, 0, 0, ny)},
		{berlin, time.Date(2027, 3, 26, 0, 0, 0, 0, berlin)},
		{berlin, time.Date(2027, 10, 29, 0, 0, 0, 0, berlin)},
		{sydney, time.Date(2027, 10, 1, 0, 0, 0, 0, sydney)},
		{sydney, time.Date(2027, 3, 31, 0, 0, 0, 0, sydney)},
	} {
		walkOnePerDay(t, c, tc.loc, tc.start)
	}
	for _, tc := range []struct {
		name string
		loc  *time.Location
		from time.Time
		want time.Time
		expr string
	}{
		{"NY gap", ny, time.Date(2027, 3, 14, 0, 0, 0, 0, ny), utc(2027, 3, 14, 7, 0), "30 2 * * *"},
		{"Berlin gap", berlin, time.Date(2027, 3, 28, 0, 0, 0, 0, berlin), utc(2027, 3, 28, 1, 0), "30 2 * * *"},
		{"Sydney gap", sydney, time.Date(2027, 10, 3, 0, 0, 0, 0, sydney), utc(2027, 10, 2, 16, 0), "30 2 * * *"},
		{"NY repeat", ny, time.Date(2027, 11, 7, 0, 0, 0, 0, ny), utc(2027, 11, 7, 5, 30), "30 1 * * *"},
		{"Berlin repeat", berlin, time.Date(2027, 10, 31, 0, 0, 0, 0, berlin), utc(2027, 10, 31, 0, 30), "30 2 * * *"},
		{"Sydney repeat", sydney, time.Date(2027, 4, 4, 0, 0, 0, 0, sydney), utc(2027, 4, 3, 15, 30), "30 2 * * *"},
		// a wall match whose earlier instant is <= after already fired: next firing is the next day
		{"NY second pass hourly", ny, utc(2027, 11, 7, 6, 10), utc(2027, 11, 8, 6, 0), "* 1 * * *"},
		{"NY second pass step", ny, utc(2027, 11, 7, 6, 2), utc(2027, 11, 8, 6, 0), "*/5 1 * * *"},
	} {
		got := nextFiring(mustCron(t, tc.expr), tc.loc, tc.from)
		if !got.Equal(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	// the repeated hour fires once: after the earlier firing, the next is the next day
	c2 := mustCron(t, "30 1 * * *")
	if again := nextFiring(c2, ny, utc(2027, 11, 7, 5, 30)); !again.Equal(utc(2027, 11, 8, 6, 30)) {
		t.Errorf("after repeated hour: %v", again)
	}
}

func TestLoadZone(t *testing.T) {
	loc, name, err := loadZone("")
	if err != nil || loc == nil {
		// A Windows dev box or a container without a symlinked /etc/localtime
		// has no IANA name for the local zone: nothing to assert about it.
		for _, cand := range localZoneCandidates() {
			if _, e := time.LoadLocation(cand); e == nil && cand != "Local" && cand != "" {
				t.Fatalf("loadZone(\"\"): %v", err)
			}
		}
		t.Skip("the server's local zone has no loadable IANA name here")
	}
	if again, err := time.LoadLocation(name); err != nil || again == nil {
		t.Errorf("stored name %q does not load: %v", name, err)
	}
	if _, name, err := loadZone("Asia/Tokyo"); err != nil || name != "Asia/Tokyo" {
		t.Errorf("loadZone(Asia/Tokyo) = %q, %v", name, err)
	}
	if _, _, err := loadZone("Not/AZone"); err == nil {
		t.Error("loadZone(Not/AZone) accepted")
	}
}

func FuzzParseCron(f *testing.F) {
	for _, s := range []string{
		"TZ=", "CRON_TZ=UTC 0 0 * * *", " TZ=Asia/Tokyo 0 9 * * *", "@every 1h",
		"0 0 30 2 *", "60 * * * *", "", "0 9 * * 1-5", "*/30 * * * *", "@daily", "0 9 13 * 5",
		"0 0 * jan,JUL mon-fri", "5/15 * * * *", "0 9 * * 7",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		c, err := parseCron(expr)
		if err != nil {
			return
		}
		if bits.OnesCount64(c.minute) == 0 || bits.OnesCount64(c.hour) == 0 {
			t.Fatalf("accepted %q with an empty field set: %+v", expr, c)
		}
		n := c.nextWall(wall(2026, 11, 15, 8, 59))
		if !n.IsZero() && !bruteMatch(c, n) {
			t.Fatalf("%q: nextWall %v does not match", expr, n)
		}
	})
}

func TestLoadZoneUnresolvableLocal(t *testing.T) {
	old := localZoneCandidates
	defer func() { localZoneCandidates = old }()
	localZoneCandidates = func() []string { return nil }
	if loc, name, err := loadZone(""); err == nil {
		t.Errorf("loadZone(\"\") = %v, %q, want an error", loc, name)
	}
	localZoneCandidates = func() []string { return []string{"Local", "Not/AZone"} }
	if _, name, err := loadZone(""); err == nil {
		t.Errorf("loadZone(\"\") accepted %q", name)
	}
	localZoneCandidates = func() []string { return []string{"Not/AZone", "Asia/Tokyo"} }
	if _, name, err := loadZone(""); err != nil || name != "Asia/Tokyo" {
		t.Errorf("loadZone(\"\") = %q, %v", name, err)
	}
}

func TestLoadZoneWithEmptyZoneinfo(t *testing.T) {
	t.Setenv("ZONEINFO", t.TempDir())
	if _, name, err := loadZone("Asia/Tokyo"); err != nil || name != "Asia/Tokyo" {
		t.Errorf("loadZone(Asia/Tokyo) with empty ZONEINFO = %q, %v", name, err)
	}
}
