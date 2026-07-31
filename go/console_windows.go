//go:build windows

package main

// The same two jobs as console_unix.go — start a shell, and be able to kill
// everything it started — with none of the same machinery available.
//
// THE SHELL. `cmd.exe /d /s /c "LINE"`, and both switches are load-bearing.
//
// Without /s, cmd applies a quote-stripping rule that depends on how many
// quotes the line contains and whether it names a real program, so
// `echo "a" "b"` and `echo "a"` are parsed by different rules. With /s the rule
// is one sentence: remove the first and last character if they are quotes, run
// the rest verbatim. That is the only form in which an arbitrary line typed on
// the dashboard reaches the shell as the person typed it.
//
// Without /d, cmd first runs whatever is in the AutoRun value under
// `HKCU\Software\Microsoft\Command Processor` — so the machine would be running
// something nobody typed here, before every single command, with its output
// mixed into the answer. Anything from a corporate profile script to a leftover
// `chcp` lands there. The dashboard asked for one command; /d is what makes it
// one command.
//
// WHAT DOES NOT WORK, said here because it looks like it should: output that is
// not ASCII may arrive mangled. A program writing to a pipe picks its own
// encoding — the OEM code page for many Windows tools — and the relay carries
// UTF-8, so those bytes become replacement characters. The obvious fix, a
// `chcp 65001` prefix, is not taken because it would not work: these commands
// run with CREATE_NO_WINDOW and therefore have no console whose code page chcp
// could set. Doing it properly means decoding per-program, which is not a thing
// that can be done correctly from this side.
//
// The command line is handed over as a single string via SysProcAttr.CmdLine
// rather than as an argv, because Windows has no argv — CreateProcess takes one
// string and every program parses it itself. Letting os/exec assemble that
// string would apply the C-runtime quoting rules to a line meant for cmd, which
// follows different ones, and the difference shows up as a backslash or a quote
// vanishing out of somebody's command.
//
// THE KILL. There are no process groups. CREATE_NEW_PROCESS_GROUP exists but
// only routes Ctrl-C, which a non-interactive service cannot send usefully, and
// killing cmd.exe alone leaves its children running with the output pipe open —
// exactly the failure the Unix side uses process groups to avoid.
//
// A JOB OBJECT is the thing that does work here. Every process the command
// starts, and every process THOSE start, is in the job; closing the last handle
// to a job created with KILL_ON_JOB_CLOSE terminates all of them at once. It is
// strictly better than walking a parent-pid tree with taskkill, which loses
// anything that got re-parented.
//
// One honest gap: the job is attached just after the process starts, not
// before, because os/exec gives no hook in between and creating the process
// suspended means reaching past it entirely. A grandchild spawned inside that
// window — microseconds, before cmd.exe has read its own command line — would
// escape the job. The Unix side has no equivalent window because setpgid
// happens inside the fork.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	// So a service running in session 0, or an interactive user watching a
	// dashboard, never gets a console window flashed at them.
	createNoWindow = 0x08000000

	jobInfoExtendedLimit   = 9
	jobLimitKillOnJobClose = 0x00002000
	processTerminate       = 0x0001
	processSetQuota        = 0x0100
)

// kernel32 is a KnownDLL: Windows resolves it from System32 and never from the
// directory the program was started in, so loading it by bare name here is not
// the DLL-planting hazard the same line would be for an ordinary library.
var (
	kernel32Console              = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32Console.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32Console.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32Console.NewProc("AssignProcessToJobObject")
)

// Layouts fixed by the Win32 headers. Written out rather than borrowed from
// x/sys/windows for the reason the whole helper has no dependencies: the offer
// is "read this, then run it", and a reader can check these against MSDN.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimit struct {
	BasicLimitInformation jobBasicLimit
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// The job each running command belongs to, so consoleKill can close it.
//
// Keyed by the command, and every entry is removed by the consoleKill that
// console.go runs unconditionally after each command — including the ones that
// succeeded. A map that only shrank on the timeout path would be a handle leak
// on the ordinary path, which is the one that runs all day.
var consoleJobs sync.Map // *exec.Cmd -> syscall.Handle

func consoleCommand(ctx context.Context, line string) *exec.Cmd {
	shell := winShell()
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// The first token is argv[0] as far as cmd.exe is concerned; it skips
		// it and parses from /d onwards.
		CmdLine:       `"` + shell + `" /d /s /c "` + line + `"`,
		CreationFlags: createNoWindow,
	}
	return cmd
}

// COMSPEC when the machine names one, System32\cmd.exe otherwise. Both are
// checked before use and there is a literal fallback, because a machine whose
// COMSPEC points at something missing should still get a working console rather
// than "spawn".
func winShell() string {
	if v := strings.TrimSpace(os.Getenv("COMSPEC")); filepath.IsAbs(v) && usableBinary(v) {
		return v
	}
	if root := strings.TrimSpace(os.Getenv("SystemRoot")); filepath.IsAbs(root) {
		if p := filepath.Join(root, "System32", "cmd.exe"); usableBinary(p) {
			return p
		}
	}
	return `C:\Windows\System32\cmd.exe`
}

// Said ONCE per process, not once per command. Failing to build the job is a
// standing property of the machine — an incompatible job it is already in,
// a privilege it does not hold — so it will fail for every command this helper
// ever runs, and a warning on each one would bury the command output the
// operator is actually reading.
var jobWarn sync.Once

// Put the running command in a kill-on-close job.
//
// Failure is survivable: the command still runs and consoleKill falls back to
// killing the shell. But it is NOT silent, because what is lost is a guarantee
// this feature states out loud — that the deadline takes down the whole tree.
// Degrading from that without saying so is the shape of failure that gets
// discovered as "why is there a stray process on that box", months later, by
// somebody who had every reason to believe it could not happen.
func consoleAdopt(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	warn := func(why string) {
		jobWarn.Do(func() {
			warnf("console: could not put commands in a job object (%s) — a timeout "+
				"will kill the shell but not what it started", why)
		})
	}
	job, err := newKillOnCloseJob()
	if err != nil {
		warn(err.Error())
		return
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		syscall.CloseHandle(job)
		warn(err.Error())
		return
	}
	// The job holds its own reference to the process; this handle was only
	// needed to name it.
	defer syscall.CloseHandle(h)
	if err := procAssignProcessToJobObject.Find(); err != nil {
		syscall.CloseHandle(job)
		warn(err.Error())
		return
	}
	if r, _, e := syscall.SyscallN(procAssignProcessToJobObject.Addr(), uintptr(job), uintptr(h)); r == 0 {
		syscall.CloseHandle(job)
		warn(e.Error())
		return
	}
	consoleJobs.Store(cmd, job)
}

func newKillOnCloseJob() (syscall.Handle, error) {
	if err := procCreateJobObjectW.Find(); err != nil {
		return 0, err
	}
	// Unnamed and with default security: this job is reached through the handle
	// and through nothing else, so there is no name for another process to open
	// and no ACL question to get wrong.
	r, _, e := syscall.SyscallN(procCreateJobObjectW.Addr(), 0, 0)
	if r == 0 {
		return 0, e
	}
	job := syscall.Handle(r)
	if err := procSetInformationJobObject.Find(); err != nil {
		syscall.CloseHandle(job)
		return 0, err
	}
	var info jobExtendedLimit
	info.BasicLimitInformation.LimitFlags = jobLimitKillOnJobClose
	ok, _, e2 := syscall.SyscallN(procSetInformationJobObject.Addr(),
		uintptr(job), jobInfoExtendedLimit,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if ok == 0 {
		// Without the limit the job would keep the tree ALIVE past the close
		// rather than killing it, which is the opposite of what this is for.
		syscall.CloseHandle(job)
		return 0, e2
	}
	return job, nil
}

// Closing the job's last handle is the kill: everything still in it goes at
// once. Returns nil when that happened, because it is a completed kill and not
// a fallback — reporting an error would put "the command failed" in front of an
// operator whose command was merely stopped on time.
func consoleKill(cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	if v, ok := consoleJobs.LoadAndDelete(cmd); ok {
		syscall.CloseHandle(v.(syscall.Handle))
		return nil
	}
	if cmd.Process == nil {
		return nil
	}
	// No job — the shell alone, and its children survive. Stated here because
	// it is the difference between the two platforms and not a thing to
	// discover from behaviour.
	return cmd.Process.Kill()
}
