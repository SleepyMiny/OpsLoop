package testenv

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// killChildrenOnExit puts this test binary in a kill-on-close job, so the kube-apiserver and etcd that envtest
// starts die with it, also when the binary is killed; otherwise every run leaks them.
func killChildrenOnExit() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return err
	}
	// The handle stays open on purpose: the OS closes it at process exit, which terminates the job.
	return windows.AssignProcessToJobObject(job, windows.CurrentProcess())
}
