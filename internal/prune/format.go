package prune

import (
	"math"
	"strconv"
	"strings"
	"time"
)

const gib = 1 << 30

// roundGB is Get-CamFreeSpaceGB's figure: [math]::Round(bytes / 1GB, 2), which is banker's
// rounding (half to even) in .NET. It feeds the watermark decision (free < reserve), so a tie
// such as 10.125 GB must come out as 10.12 here too (IR section 10 item 15).
func roundGB(bytes float64) float64 {
	return math.RoundToEven(bytes/gib*100) / 100
}

// netFixed formats v like .NET Framework's Double.ToString("0.00..", InvariantCulture): the value
// is first taken to 15 significant digits, then rounded half away from zero to the given number
// of decimals. Messages only; no decision depends on it.
func netFixed(v float64, decimals int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	neg := v < 0
	e := strconv.FormatFloat(math.Abs(v), 'e', 14, 64) // d.dddddddddddddde+XX
	mant, expText, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expText)

	// The scaled integer is digits x 10^k.
	k := exp - 14 + decimals
	var n string
	switch {
	case k >= 0:
		n = digits + strings.Repeat("0", k)
	case -k > len(digits):
		n = "0"
	default:
		cut := len(digits) + k
		n = digits[:cut]
		if digits[cut] >= '5' {
			n = incDecimal(n)
		}
		if n == "" {
			n = "0"
		}
	}
	n = strings.TrimLeft(n, "0")
	zero := n == ""
	for len(n) <= decimals {
		n = "0" + n
	}
	out := n
	if decimals > 0 {
		out = n[:len(n)-decimals] + "." + n[len(n)-decimals:]
	}
	if neg && !zero {
		out = "-" + out
	}
	return out
}

func incDecimal(s string) string {
	b := []byte(s)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < '9' {
			b[i]++
			return string(b)
		}
		b[i] = '0'
	}
	return "1" + string(b)
}

// netTrim is the "0.##" form: up to the given decimals, trailing zeros dropped.
func netTrim(v float64, decimals int) string {
	s := netFixed(v, decimals)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func n2(v float64) string { return netFixed(v, 2) }

// stamp is the PowerShell log stamp of a file time: local time, "yyyy-MM-dd HH:mm:ss".
func stamp(t time.Time) string { return t.In(time.Local).Format("2006-01-02 15:04:05") }

// days is how PowerShell interpolates a [double] into a string ("1", "0.5").
func days(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
