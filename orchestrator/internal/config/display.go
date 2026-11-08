package config

import (
	"reflect"
	"regexp"
	"strings"

	"github.com/0xyu/aco/internal/secure"
)

// secretKeyNames are credential-like key names a diagnostic may repeat when
// it rejects them (ErrSecretField). Only these exact names (any case) are
// shown; any other secret-looking key is hidden, since it may itself be a
// pasted credential.
var secretKeyNames = map[string]bool{
	"api_key": true, "api-key": true, "apikey": true,
	"secret": true, "client_secret": true,
	"password": true, "passwd": true,
	"token": true, "access_token": true, "auth_token": true, "api_token": true,
	"credential": true, "credentials": true,
	"private_key": true, "private-key": true,
}

var segIndexRE = regexp.MustCompile(`^([^\[\]]*)((?:\[[0-9]+\])*)$`)

// DisplayPath renders a dotted field path for a diagnostic using only
// trusted parts: field names from the Config schema, list indexes, and the
// well-known secret key names above. Every other segment (an unknown key or
// a map key, both written by the user) becomes secure.Hidden; nothing after
// an unknown key is shown.
func DisplayPath(path string) string {
	if path == "" {
		return ""
	}
	t := reflect.TypeOf(Config{})
	var out []string
	for _, seg := range strings.Split(path, ".") {
		m := segIndexRE.FindStringSubmatch(seg)
		switch {
		case t != nil && t.Kind() == reflect.Map:
			// The key is user-chosen; its value type is still known.
			out = append(out, secure.Hidden)
			t = t.Elem()
			continue
		case t != nil && t.Kind() == reflect.Struct && m != nil:
			if ft, ok := yamlFields(t)[m[1]]; ok && (m[2] == "" || ft.Kind() == reflect.Slice) {
				out = append(out, seg)
				t = ft
				if m[2] != "" {
					t = t.Elem()
				}
				continue
			}
		}
		if m != nil && m[2] == "" && secretKeyNames[strings.ToLower(seg)] {
			out = append(out, seg)
		} else {
			out = append(out, secure.Hidden)
		}
		break
	}
	return strings.Join(out, ".")
}

// overrideSource names a --set argument for diagnostics and layer listings:
// "--set <key>" when every key segment is trusted (see DisplayPath), else
// "--set #<n>" by position, since the key itself may be a credential.
func overrideSource(ordinal, key string) string {
	if DisplayPath(key) == key {
		return "--set " + key
	}
	return "--set " + ordinal
}
