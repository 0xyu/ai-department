package auth

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// EnvFileError is a syntax error in an env file. It never contains the
// offending value, because the value may be a secret.
type EnvFileError struct {
	Path string
	Line int
}

func (e *EnvFileError) Error() string {
	return fmt.Sprintf("%s:%d: invalid env file line (expected KEY=VALUE)", e.Path, e.Line)
}

// EnvFile is a parsed env file.
type EnvFile struct {
	Path   string
	Found  bool
	Values map[string]string
	// LoosePerms is true on Unix when group/others can read the file.
	LoosePerms bool
}

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExpandHome expands a leading "~" or "~/" using home.
func ExpandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return filepath.Join(home, path[2:])
	}
	return path
}

// LoadEnvFile reads and parses path. A missing file is an error only when
// mustExist is true (an explicit --env-file); otherwise Found is false.
func LoadEnvFile(path string, mustExist bool) (*EnvFile, error) {
	ef := &EnvFile{Path: path, Values: map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !mustExist {
			return ef, nil
		}
		return nil, err
	}
	ef.Found = true
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
			ef.LoosePerms = true
		}
	}
	vals, err := ParseEnv(data, path)
	if err != nil {
		return nil, err
	}
	ef.Values = vals
	return ef, nil
}

// ParseEnv parses dotenv syntax: KEY=VALUE lines, "#" comments, optional
// "export " prefix, single quotes (literal), double quotes (\n \t \" \\ escapes),
// and inline " #" comments after unquoted values.
func ParseEnv(data []byte, path string) (map[string]string, error) {
	out := map[string]string{}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !envKeyRE.MatchString(key) {
			return nil, &EnvFileError{Path: path, Line: lineNo}
		}
		v, ok := parseEnvValue(strings.TrimSpace(val))
		if !ok {
			return nil, &EnvFileError{Path: path, Line: lineNo}
		}
		out[key] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func parseEnvValue(v string) (string, bool) {
	if v == "" {
		return "", true
	}
	switch v[0] {
	case '\'':
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", false
		}
		return v[1 : 1+end], true
	case '"':
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			c := v[i]
			if c == '"' {
				return b.String(), true
			}
			if c == '\\' && i+1 < len(v) {
				i++
				switch v[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				default:
					b.WriteByte(v[i])
				}
				continue
			}
			b.WriteByte(c)
		}
		return "", false // unterminated
	default:
		if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		return v, true
	}
}
