package cli

import (
	"flag"
	"strings"
	"unicode"

	"github.com/0xyu/aco/internal/i18n"
)

// displayWidth counts East Asian wide characters as two columns.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || (r >= 0xFF00 && r <= 0xFFEF) || (r >= 0x3000 && r <= 0x303F) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func pad(s string, width int) string {
	if d := width - displayWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

type row struct{ left, right string }

func printRows(p *i18n.Printer, rows []row) {
	width := 0
	for _, r := range rows {
		if w := displayWidth(r.left); w > width {
			width = w
		}
	}
	for _, r := range rows {
		p.Say(i18n.CLIHelpRow, pad(r.left, width), r.right)
	}
}

func printRootHelp(p *i18n.Printer) {
	p.Say(i18n.CLIRootTitle)
	p.Blank()
	p.Say(i18n.CLIUsageHeader)
	p.Say(i18n.CLIRootUsageLine)
	p.Blank()
	p.Say(i18n.CLICommandsHeader)
	var rows []row
	for _, c := range commands() {
		desc := p.T(c.summary)
		if c.internal {
			desc += p.T(i18n.CLIInternalMark)
		}
		rows = append(rows, row{c.name, desc})
	}
	printRows(p, rows)
	p.Blank()
	p.Say(i18n.CLIExitCodes)
	p.Say(i18n.CLIRootFooter)
}

func printCommandHelp(p *i18n.Printer, c *command, fs *flag.FlagSet) {
	args := i18n.CLIArgsFlagsOnly
	if c.args != "" {
		args = c.args
	}
	p.Say(i18n.CLIUsageHeader)
	p.Say(i18n.CLICommandUsage, c.name, i18n.M(args))
	p.Blank()
	desc := p.T(c.summary)
	if c.internal {
		desc += p.T(i18n.CLIInternalMark)
	}
	p.Say(i18n.CLIHelpRow, "", desc)
	p.Blank()
	p.Say(i18n.CLIFlagsHeader)
	var rows []row
	for _, name := range c.flags {
		d := flagDefs[name]
		left := "--" + name
		if d.placeholder != "" {
			left += " " + p.T(d.placeholder)
		}
		right := p.T(d.usage)
		if d.def != "" {
			right += " (" + d.def + ")"
		}
		rows = append(rows, row{left, right})
	}
	rows = append(rows, row{"-h, --help", p.T(i18n.FlagHelp)})
	printRows(p, rows)
}
