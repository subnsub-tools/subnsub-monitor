//go:build windows

package main

// Antigravity is NOT discovered on Windows, and this file exists to say so out
// loud rather than let a silent nil be read as "the IDE is not running".
//
// What the other two platforms do is find the language server's process, take
// the --csrf_token out of its command line, work out which loopback port that
// same process is listening on, and ask it. Windows can answer the second half
// of that easily — GetExtendedTcpTable maps a listening port to a pid in one
// call — and refuses the first half.
//
// Reading another process's command line on Windows means one of:
//
//	WMI / CIM        Win32_Process has CommandLine. Reaching it means COM, or
//	                 shelling out to PowerShell and paying a second or two of
//	                 startup on a 30-second push loop. (wmic.exe, the cheap way,
//	                 was removed from Windows 11 24H2.)
//	NtQueryInformationProcess + ReadProcessMemory
//	                 Walk the target's PEB and read its RTL_USER_PROCESS_PARAMETERS.
//	                 It works, and it is an undocumented struct whose layout has
//	                 moved between Windows versions and differs again for a
//	                 32-bit process seen from a 64-bit one.
//
// The first is too expensive for the loop it would run in. The second asks a
// helper whose entire claim is "it reads files and makes one outbound request"
// to start reading other processes' memory — which is a much larger thing to be
// on somebody's machine, and it would be there for every user whether or not
// they have ever opened Antigravity.
//
// So this platform reports no Antigravity reading, which the page renders as
// the provider being absent — the same as a machine that never installed it.
// Codex, Amp and Claude Code all work here: their quota is in a file or behind
// a CLI, and neither needs anyone's process table.

func agCandidates() []agCandidate { return nil }
