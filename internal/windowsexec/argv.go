// Package windowsexec adapts guest commands to the Incus Windows agent's CRT quoting.
package windowsexec

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

const utf16UnitBytes = 2

// PowerShell returns an encoded script invocation without quoted argv elements.
func PowerShell(script string) []string {
	encoded := make([]byte, 0, len(script)*utf16UnitBytes)
	var units [2]uint16
	for _, r := range script {
		for _, unit := range utf16.AppendRune(units[:0], r) {
			encoded = binary.LittleEndian.AppendUint16(encoded, unit)
		}
	}
	return []string{
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-EncodedCommand",
		base64.StdEncoding.EncodeToString(encoded),
	}
}

// Command runs a CMD command with native quoting, environment expansion, and inherited streams.
func Command(command string) []string {
	// Incus's Windows agent applies CRT quoting, which is not CMD quoting.
	// ProcessStartInfo.Arguments passes the raw command line to CMD instead.
	script := "$p=[System.Diagnostics.ProcessStartInfo]::new();" +
		"$p.FileName='cmd.exe';$p.Arguments='/d /s /c \"'+'" + strings.ReplaceAll(command, "'", "''") + "'+'\"';" +
		"$p.UseShellExecute=$false;$child=[System.Diagnostics.Process]::Start($p);" +
		"$child.WaitForExit();exit $child.ExitCode"
	return PowerShell(script)
}
