package cli

import (
	"errors"
	"io/fs"
	"strings"

	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/doctor"
	"github.com/0xyu/aco/internal/i18n"
)

// renderFlagError maps the standard flag package's English errors to catalog
// messages. It never forwards the package's text for a rejected value: that
// text embeds the value quoted and escaped, which could be a credential the
// literal secret filter no longer recognizes.
func renderFlagError(p *i18n.Printer, err error, bad badValue) {
	if bad.name != "" {
		typ := i18n.CLITypeInteger
		if bad.kind == kindBool {
			typ = i18n.CLITypeBoolean
		}
		p.Say(i18n.CLIErrFlagBadValue, bad.name, i18n.M(typ))
		return
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "flag provided but not defined: "):
		p.Say(i18n.CLIErrUnknownFlag, strings.TrimPrefix(msg, "flag provided but not defined: "))
	case strings.HasPrefix(msg, "flag needs an argument: "):
		p.Say(i18n.CLIErrFlagNeedsArg, strings.TrimPrefix(msg, "flag needs an argument: "))
	default:
		// e.g. "bad flag syntax: <token>": the token may be a secret.
		p.Say(i18n.CLIErrFlagInvalid)
	}
}

func location(e config.FieldError) i18n.Msg {
	switch {
	case e.Source != "" && e.Line > 0:
		return i18n.M(i18n.ConfigErrLine, e.Source, e.Line)
	case e.Source != "":
		return i18n.M(i18n.ConfigErrLoc, e.Source)
	case e.Layer == config.LayerDefaults || e.Layer == "":
		return i18n.M(i18n.ConfigLayerDefaults)
	default:
		return i18n.M(i18n.ConfigErrLoc, e.Layer)
	}
}

// fieldErrorMsg turns a config error into a catalog message.
func fieldErrorMsg(e config.FieldError) i18n.Msg {
	loc := location(e)
	switch e.Code {
	case config.ErrParse:
		return i18n.M(i18n.ConfigErrParse, loc, e.Value)
	case config.ErrUnknownField:
		return i18n.M(i18n.ConfigErrUnknownField, loc, e.Field)
	case config.ErrUnknownGateType:
		return i18n.M(i18n.ConfigErrUnknownGateType, loc, e.Value, e.Allowed)
	case config.ErrSecretField:
		return i18n.M(i18n.ConfigErrSecretField, loc, e.Field)
	case config.ErrInvalidType:
		if e.Field == "" {
			return i18n.M(i18n.ConfigErrDecode, loc, e.Value)
		}
		return i18n.M(i18n.ConfigErrInvalidType, loc, e.Field, e.Allowed)
	case config.ErrInvalidValue:
		return i18n.M(i18n.ConfigErrInvalidValue, loc, e.Field, e.Value, e.Allowed)
	case config.ErrOutOfRange:
		return i18n.M(i18n.ConfigErrOutOfRange, loc, e.Field, e.Value, e.Allowed)
	case config.ErrInvalidDuration:
		return i18n.M(i18n.ConfigErrInvalidDuration, loc, e.Field, e.Value)
	case config.ErrUnsupportedVersion:
		return i18n.M(i18n.ConfigErrUnsupportedVersion, loc, e.Value, e.Allowed)
	case config.ErrRequired:
		return i18n.M(i18n.ConfigErrRequired, loc, e.Field)
	case config.ErrFileNotFound:
		return i18n.M(i18n.ConfigErrFileNotFound, e.Source)
	case config.ErrInvalidOverride:
		return i18n.M(i18n.ConfigErrInvalidOverride, e.Value)
	default:
		return i18n.M(i18n.CLIErrInternal, e.Error())
	}
}

func renderConfigError(p *i18n.Printer, err error) {
	var ce config.Errors
	if !errors.As(err, &ce) {
		p.Say(i18n.CLIErrInternal, err.Error())
		return
	}
	p.Say(i18n.ConfigErrHeader)
	for _, e := range ce {
		p.Say(i18n.ConfigErrBullet, fieldErrorMsg(e))
	}
}

func renderAuthError(p *i18n.Printer, err error) {
	var ae auth.Errors
	if !errors.As(err, &ae) {
		p.Say(i18n.CLIErrInternal, err.Error())
		return
	}
	p.Say(i18n.AuthErrHeader)
	for _, e := range ae {
		p.Say(i18n.ConfigErrBullet, i18n.M(i18n.AuthErrInvalid, doctor.SourceMsg(e.Source), e.Value, e.Allowed))
	}
}

func renderEnvFileError(p *i18n.Printer, path string, err error) {
	var se *auth.EnvFileError
	switch {
	case errors.As(err, &se):
		p.Say(i18n.EnvFileErrSyntax, se.Path, se.Line)
	case isNotExist(err):
		p.Say(i18n.EnvFileErrNotFound, path)
	default:
		p.Say(i18n.EnvFileErrRead, path, err.Error())
	}
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
