package server

import (
	"errors"
	"fmt"
	"math/bits"
	"os"
	"strings"
	"time"
	_ "time/tzdata" // zone names load on hosts without a zone database (S-D3)

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// cronExpr is a parsed 5-field cron expression as bitsets (bit n = value n).
// dow bit 7 is folded into bit 0. domStar/dowStar record that the field's text
// began with '*', which selects the day rule (Vixie): if either is set a day
// must match both fields, otherwise either.
type cronExpr struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

var cronDescriptors = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var (
	cronMonthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	cronDowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
)

// parseCron is the only way an expression becomes a schedule (S-D2): a
// descriptor or five space-separated fields, nothing more. It also refuses an
// expression with no match within five years from now.
func parseCron(expr string) (cronExpr, error) {
	expr = strings.TrimSpace(expr)
	if len(expr) > protocol.ScheduleCronMax {
		return cronExpr{}, fmt.Errorf("cron expression longer than %d bytes", protocol.ScheduleCronMax)
	}
	if strings.HasPrefix(expr, "@") {
		d, ok := cronDescriptors[strings.ToLower(expr)]
		if !ok {
			return cronExpr{}, fmt.Errorf("unknown descriptor %q", expr)
		}
		expr = d
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return cronExpr{}, fmt.Errorf("cron expression needs 5 fields, got %d", len(f))
	}
	var c cronExpr
	var err error
	specs := []struct {
		name   string
		text   string
		lo, hi int
		names  map[string]int
		dst    *uint64
	}{
		{"minute", f[0], 0, 59, nil, &c.minute},
		{"hour", f[1], 0, 23, nil, &c.hour},
		{"day-of-month", f[2], 1, 31, nil, &c.dom},
		{"month", f[3], 1, 12, cronMonthNames, &c.month},
		{"day-of-week", f[4], 0, 7, cronDowNames, &c.dow},
	}
	for _, sp := range specs {
		if *sp.dst, err = parseCronField(sp.text, sp.lo, sp.hi, sp.names); err != nil {
			return cronExpr{}, fmt.Errorf("%s field %q: %w", sp.name, sp.text, err)
		}
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday
		c.dow = c.dow&^(1<<7) | 1
	}
	c.domStar = strings.HasPrefix(f[2], "*")
	c.dowStar = strings.HasPrefix(f[4], "*")
	if c.nextWall(wallClockNow()).IsZero() {
		return cronExpr{}, fmt.Errorf("cron expression %q never fires within 5 years", expr)
	}
	return c, nil
}

func wallClockNow() time.Time { return time.Now().UTC() }

func parseCronNumber(s string, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	if s == "" || len(s) > 3 {
		return 0, fmt.Errorf("bad number %q", s)
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("bad number %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func parseCronField(text string, lo, hi int, names map[string]int) (uint64, error) {
	var set uint64
	for _, item := range strings.Split(text, ",") {
		base, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			var err error
			if step, err = parseCronNumber(stepText, nil); err != nil {
				return 0, err
			}
			if step < 1 {
				return 0, errors.New("step must be at least 1")
			}
		}
		var a, b int
		if base == "*" {
			a, b = lo, hi
		} else {
			first, last, isRange := strings.Cut(base, "-")
			var err error
			if a, err = parseCronNumber(first, names); err != nil {
				return 0, err
			}
			switch {
			case isRange:
				if b, err = parseCronNumber(last, names); err != nil {
					return 0, err
				}
			case hasStep:
				b = hi
			default:
				b = a
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("range %d-%d outside %d-%d", a, b, lo, hi)
		}
		for v := a; v <= b; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, nil
}

// dayMatches applies the day rule to the calendar day of t.
func (c cronExpr) dayMatches(t time.Time) bool {
	dom := c.dom>>uint(t.Day())&1 == 1
	dow := c.dow>>uint(t.Weekday())&1 == 1
	if c.domStar || c.dowStar {
		return dom && dow
	}
	return dom || dow
}

// firstBit returns the smallest set bit of mask that is >= from, or -1.
func firstBit(mask uint64, from int) int {
	if from > 63 {
		return -1
	}
	m := mask >> uint(from) << uint(from)
	if m == 0 {
		return -1
	}
	return bits.TrailingZeros64(m)
}

// nextWall: after is wall-clock fields carried in UTC; the result is the
// smallest whole minute strictly after it that matches, or zero if none within
// five years.
func (c cronExpr) nextWall(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	y, mo, d := t.Date()
	for i := 0; i <= 5*366+1; i++ {
		day := time.Date(y, mo, d+i, 0, 0, 0, 0, time.UTC)
		if c.month>>uint(day.Month())&1 == 0 || !c.dayMatches(day) {
			continue
		}
		h, mi := 0, 0
		if i == 0 {
			h, mi = t.Hour(), t.Minute()
		}
		for h < 24 {
			next := firstBit(c.hour, h)
			if next < 0 {
				break
			}
			if next != h {
				mi = 0 // a later hour than the constrained one: any minute
			}
			h = next
			if m := firstBit(c.minute, mi); m >= 0 {
				return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
			}
			h, mi = h+1, 0
		}
	}
	return time.Time{}
}

// nextFiring is the first firing of c strictly after `after`, as a UTC
// instant; zero means none. The expression is matched against local wall-clock
// time (S-D3a): nextWall runs on the wall-clock fields carried in UTC (no
// transitions) and each match is mapped back into loc. A match in a
// spring-forward gap fires at the end of the gap; a match in a repeated hour
// fires once, at the earlier instant (wallToInstant's rule: time.Date's choice
// depends on the sign of the zone's offset, S-D3a).
func nextFiring(c cronExpr, loc *time.Location, after time.Time) time.Time {
	w := after.In(loc)
	cur := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC)
	// A repeated hour of a 2h shift holds up to 120 matches of "* h * * *",
	// all of which may map at or before `after`; the cap covers that with room.
	for range 300 {
		m := c.nextWall(cur)
		if m.IsZero() {
			return time.Time{}
		}
		if t := wallToInstant(m, loc); t.After(after) {
			return t.UTC()
		}
		cur = m
	}
	return time.Time{}
}

// wallToInstant maps wall-clock fields (carried in UTC) to an instant in loc:
// the earlier instant if the wall time occurs twice, and the transition
// instant if it does not occur (S-D3a). time.Date cannot be used: its choice in
// both cases depends on the sign of the zone's offset.
func wallToInstant(m time.Time, loc *time.Location) time.Time {
	_, offBefore := m.Add(-24 * time.Hour).In(loc).Zone()
	_, offAfter := m.Add(24 * time.Hour).In(loc).Zone()
	var best time.Time
	for _, off := range []int{offBefore, offAfter} {
		cand := m.Add(-time.Duration(off) * time.Second)
		l := cand.In(loc)
		if l.Year() != m.Year() || l.Month() != m.Month() || l.Day() != m.Day() ||
			l.Hour() != m.Hour() || l.Minute() != m.Minute() {
			continue
		}
		if best.IsZero() || cand.Before(best) {
			best = cand
		}
	}
	if !best.IsZero() {
		return best
	}
	// In a gap: the wall time read with the smaller (pre-transition) offset
	// lies just after the transition; its zone period starts at it.
	off := min(offBefore, offAfter)
	start, _ := m.Add(-time.Duration(off) * time.Second).In(loc).ZoneBounds()
	return start
}

// maxZoneName bounds the zone field (control messages are one datagram).
const maxZoneName = 64

// loadZone resolves "" to the server's local zone by IANA name ($TZ, then the
// /etc/localtime symlink target) and refuses when no name can be determined:
// "Local" would follow the host's zone later, so it is never stored. Every
// returned name loads again with time.LoadLocation on this host.
func loadZone(name string) (*time.Location, string, error) {
	if len(name) > maxZoneName {
		return nil, "", fmt.Errorf("time zone name longer than %d bytes", maxZoneName)
	}
	if name == "" {
		for _, cand := range localZoneCandidates() {
			if cand == "Local" || cand == "" {
				continue
			}
			if loc, err := time.LoadLocation(cand); err == nil {
				return loc, cand, nil
			}
		}
		return nil, "", errors.New("the server's local time zone has no IANA name it can determine; give the zone explicitly (--tz)")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, "", fmt.Errorf("time zone %q: %w", name, err)
	}
	return loc, name, nil
}

// localZoneCandidates is a variable so tests can make the local zone unresolvable.
var localZoneCandidates = func() []string {
	var out []string
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		out = append(out, tz)
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			out = append(out, target[i+len("zoneinfo/"):])
		}
	}
	return out
}
