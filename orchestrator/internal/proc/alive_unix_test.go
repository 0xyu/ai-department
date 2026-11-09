//go:build !windows

package proc

import (
	"os/exec"
	"strings"
)

// alive reports whether pid runs and is not a zombie.
func alive(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", itoa(pid)).Output()
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(out))
	return s != "" && !strings.HasPrefix(s, "Z")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
