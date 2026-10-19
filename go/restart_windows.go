//go:build windows

package main

// Windows keeps the arrangement this change replaced everywhere else, and that
// is a decision rather than an omission.
//
// The bug being fixed is that a machine may have NO supervisor: a container
// whose PID 1 supervises nothing, with a Python stand-in answering to the name
// systemctl. Windows has no equivalent of that. The installer registers a
// Scheduled Task, the task is the real Task Scheduler, and its repetition
// trigger genuinely does start the helper again — so the failure this file
// would be working around does not exist here.
//
// What DOES exist here is a way to make things worse, and it is worth naming
// so nobody "fixes" this later by symmetry:
//
//   - A process created with CreateProcess inherits its parent's Job object.
//     A task instance runs in a job the scheduler holds a handle to, and when
//     the parent exits the scheduler treats the instance as finished and closes
//     it. With kill-on-close set, the child we so carefully detached dies with
//     it. DETACHED_PROCESS does not help: it detaches a console, not a job.
//     CREATE_BREAKAWAY_FROM_JOB is the flag that would, and it makes
//     CreateProcess FAIL outright unless the job was created permitting
//     breakaway — which is not ours to decide.
//   - A helper started by hand is not the task's instance, so the task's own
//     "do not start if already running" does not cover it, and the next trigger
//     starts a second helper beside the one we spawned.
//
// Both of those are ways to turn a working update into a broken machine, and
// neither is testable from the platform this is developed on. So the exit
// stays, the task does the restart, and the sentinel below says so plainly
// rather than letting restartSelf log a failure that is not one.
func restartInto(self string) error {
	return errSupervisorOwnsRestart
}
