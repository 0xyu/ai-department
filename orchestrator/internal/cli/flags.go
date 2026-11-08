package cli

import (
	"errors"
	"strconv"
)

// errBadFlagValue is returned by typed flag values. It carries no text from
// the rejected value: the flag package wraps it with the value quoted and
// escaped, which the literal secret filter cannot recognize, so the CLI never
// prints that message (see badValue).
var errBadFlagValue = errors.New("invalid flag value")

// badValue records the first flag whose value failed to parse.
type badValue struct {
	name string
	kind flagKind
}

// intFlag is an int flag that reports parse failures through *badValue.
type intFlag struct {
	p    *int
	name string
	bad  *badValue
}

func (f *intFlag) String() string {
	if f == nil || f.p == nil {
		return "0"
	}
	return strconv.Itoa(*f.p)
}

func (f *intFlag) Set(s string) error {
	n, err := strconv.ParseInt(s, 0, strconv.IntSize)
	if err != nil {
		f.bad.record(f.name, kindInt)
		return errBadFlagValue
	}
	*f.p = int(n)
	return nil
}

// boolFlag is a bool flag that reports parse failures through *badValue.
type boolFlag struct {
	p    *bool
	name string
	bad  *badValue
}

func (f *boolFlag) String() string {
	if f == nil || f.p == nil {
		return "false"
	}
	return strconv.FormatBool(*f.p)
}

func (f *boolFlag) Set(s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		f.bad.record(f.name, kindBool)
		return errBadFlagValue
	}
	*f.p = b
	return nil
}

// IsBoolFlag lets "--retry" stand alone without a value.
func (f *boolFlag) IsBoolFlag() bool { return true }

func (b *badValue) record(name string, kind flagKind) {
	if b.name == "" {
		b.name, b.kind = name, kind
	}
}
