//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	localSystemSID                    = "S-1-5-18"
	sePrivilegeEnabled                = 0x00000002
	securityImpersonation             = 2
	tokenPrimary                      = 1
	createNoWindow                    = 0x08000000
	processSetQuota                   = 0x0100
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	invalidSessionID                  = ^uint32(0)
)

var (
	modadvapi32 = syscall.NewLazyDLL("advapi32.dll")
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")
	modwtsapi32 = syscall.NewLazyDLL("wtsapi32.dll")
	moduserenv  = syscall.NewLazyDLL("userenv.dll")

	procLookupPrivilegeValueW        = modadvapi32.NewProc("LookupPrivilegeValueW")
	procAdjustTokenPrivileges        = modadvapi32.NewProc("AdjustTokenPrivileges")
	procDuplicateTokenEx             = modadvapi32.NewProc("DuplicateTokenEx")
	procSetTokenInformation          = modadvapi32.NewProc("SetTokenInformation")
	procWTSGetActiveConsoleSessionId = modkernel32.NewProc("WTSGetActiveConsoleSessionId")
	procProcessIdToSessionId         = modkernel32.NewProc("ProcessIdToSessionId")
	procCreateJobObjectW             = modkernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject      = modkernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject     = modkernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess                  = modkernel32.NewProc("OpenProcess")
	procWTSQueryUserToken            = modwtsapi32.NewProc("WTSQueryUserToken")
	procCreateEnvironmentBlock       = moduserenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock      = moduserenv.NewProc("DestroyEnvironmentBlock")
)

type luid struct {
	lowPart  uint32
	highPart int32
}

type luidAndAttributes struct {
	luid       luid
	attributes uint32
}

type tokenPrivileges struct {
	privilegeCount uint32
	privileges     [1]luidAndAttributes
}

type jobObjectBasicLimitInformation struct {
	perProcessUserTimeLimit int64
	perJobUserTimeLimit     int64
	limitFlags              uint32
	minimumWorkingSetSize   uintptr
	maximumWorkingSetSize   uintptr
	activeProcessLimit      uint32
	affinity                uintptr
	priorityClass           uint32
	schedulingClass         uint32
}

type ioCounters struct {
	readOperationCount  uint64
	writeOperationCount uint64
	otherOperationCount uint64
	readTransferCount   uint64
	writeTransferCount  uint64
	otherTransferCount  uint64
}
type jobExtendedLimitInfo struct {
	basicLimitInformation jobObjectBasicLimitInformation
	ioInfo                ioCounters
	processMemoryLimit    uintptr
	jobMemoryLimit        uintptr
	peakProcessMemoryUsed uintptr
	peakJobMemoryUsed     uintptr
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cua-driver-proxy CUA-DRIVER-ARGS...")
		os.Exit(2)
	}
	if err := run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "cua-driver-proxy:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := requireLocalSystem(); err != nil {
		return err
	}
	if err := requireSessionZero(); err != nil {
		return err
	}

	var processToken syscall.Token
	current, err := syscall.GetCurrentProcess()
	if err != nil {
		return fmt.Errorf("current process: %w", err)
	}
	if err := syscall.OpenProcessToken(
		current,
		syscall.TOKEN_QUERY|syscall.TOKEN_ADJUST_PRIVILEGES,
		&processToken,
	); err != nil {
		return fmt.Errorf("open service token: %w", err)
	}
	defer processToken.Close()
	for _, name := range []string{"SeTcbPrivilege", "SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege"} {
		if err := enablePrivilege(processToken, name); err != nil {
			return fmt.Errorf("enable %s: %w", name, err)
		}
	}

	session := wtsGetActiveConsoleSessionID()
	if session == invalidSessionID || session == 0 {
		return errors.New("no interactive console session")
	}
	var interactive syscall.Token
	if err := wtsQueryUserToken(session, &interactive); err != nil {
		return fmt.Errorf("query console user token for session %d: %w", session, err)
	}
	defer interactive.Close()

	var primary syscall.Token
	access := uint32(
		syscall.TOKEN_ASSIGN_PRIMARY | syscall.TOKEN_DUPLICATE | syscall.TOKEN_QUERY | syscall.TOKEN_ADJUST_DEFAULT | syscall.TOKEN_ADJUST_SESSIONID,
	)
	if err := duplicateTokenEx(interactive, access, securityImpersonation, tokenPrimary, &primary); err != nil {
		return fmt.Errorf("duplicate console user token: %w", err)
	}
	defer primary.Close()

	// Cua authenticates the account SID. Keep the client in Session 0 because
	// Windows does not allow stdio handle inheritance across sessions.
	var serviceSession uint32
	if err := setTokenSessionID(primary, serviceSession); err != nil {
		return fmt.Errorf("set proxy session: %w", err)
	}

	environment, err := tokenEnviron(primary)
	if err != nil {
		return fmt.Errorf("load console user environment: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(filepath.Join(filepath.Dir(self), "cua-driver.exe"), os.Args[1:]...)
	command.Env = environment
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		Token:         syscall.Token(primary),
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return runWithJob(command)
}

func requireLocalSystem() error {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return fmt.Errorf("open process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("query token user: %w", err)
	}
	sid, err := user.User.Sid.String()
	if err != nil {
		return fmt.Errorf("token sid: %w", err)
	}
	if sid != localSystemSID {
		return fmt.Errorf("must run as Local System (%s), got %s", localSystemSID, sid)
	}
	return nil
}

func requireSessionZero() error {
	var session uint32
	r1, _, e1 := procProcessIdToSessionId.Call(uintptr(os.Getpid()), uintptr(unsafe.Pointer(&session)))
	if r1 == 0 {
		return fmt.Errorf("process session: %w", errno(e1))
	}
	if session != 0 {
		return fmt.Errorf("must run in Session 0, got session %d", session)
	}
	return nil
}

func enablePrivilege(token syscall.Token, name string) error {
	value, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var id luid
	r1, _, e1 := procLookupPrivilegeValueW.Call(0, uintptr(unsafe.Pointer(value)), uintptr(unsafe.Pointer(&id)))
	if r1 == 0 {
		return errno(e1)
	}
	state := tokenPrivileges{privilegeCount: 1}
	state.privileges[0] = luidAndAttributes{luid: id, attributes: sePrivilegeEnabled}
	r1, _, e1 = procAdjustTokenPrivileges.Call(uintptr(token), 0, uintptr(unsafe.Pointer(&state)), 0, 0, 0)
	if r1 == 0 {
		return errno(e1)
	}
	return nil
}

func wtsGetActiveConsoleSessionID() uint32 {
	r0, _, _ := procWTSGetActiveConsoleSessionId.Call()
	return uint32(r0)
}

func wtsQueryUserToken(session uint32, token *syscall.Token) error {
	r1, _, e1 := procWTSQueryUserToken.Call(uintptr(session), uintptr(unsafe.Pointer(token)))
	if r1 == 0 {
		return errno(e1)
	}
	return nil
}

func duplicateTokenEx(
	existing syscall.Token,
	access uint32,
	impersonationLevel, tokenType uint32,
	primary *syscall.Token,
) error {
	r1, _, e1 := procDuplicateTokenEx.Call(
		uintptr(existing),
		uintptr(access),
		0,
		uintptr(impersonationLevel),
		uintptr(tokenType),
		uintptr(unsafe.Pointer(primary)),
	)
	if r1 == 0 {
		return errno(e1)
	}
	return nil
}

func setTokenSessionID(token syscall.Token, session uint32) error {
	r1, _, e1 := procSetTokenInformation.Call(
		uintptr(token),
		uintptr(syscall.TokenSessionId),
		uintptr(unsafe.Pointer(&session)),
		unsafe.Sizeof(session),
	)
	if r1 == 0 {
		return errno(e1)
	}
	return nil
}

func tokenEnviron(token syscall.Token) ([]string, error) {
	var block *uint16
	r1, _, e1 := procCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&block)), uintptr(token), 0)
	if r1 == 0 {
		return nil, errno(e1)
	}
	defer procDestroyEnvironmentBlock.Call(uintptr(unsafe.Pointer(block)))

	var env []string
	size := unsafe.Sizeof(*block)
	for *block != 0 {
		end := unsafe.Pointer(block)
		for *(*uint16)(end) != 0 {
			end = unsafe.Add(end, size)
		}
		entry := unsafe.Slice(block, (uintptr(end)-uintptr(unsafe.Pointer(block)))/size)
		env = append(env, syscall.UTF16ToString(entry))
		block = (*uint16)(unsafe.Add(end, size))
	}
	return env, nil
}

func runWithJob(command *exec.Cmd) error {
	// Job with KILL_ON_JOB_CLOSE so a cancelled Incus exec does not leave a
	// Session-0 cua-driver client behind. cua-driver mcp also exits on stdin
	// EOF; this covers TerminateProcess of the proxy.
	job, err := createKillJob()
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(job)

	if err := command.Start(); err != nil {
		return err
	}
	if err := assignPIDToJob(job, command.Process.Pid); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("assign child to cleanup job: %w", err)
	}
	return command.Wait()
}

func createKillJob() (syscall.Handle, error) {
	r0, _, e1 := procCreateJobObjectW.Call(0, 0)
	if r0 == 0 {
		return 0, fmt.Errorf("create job: %w", errno(e1))
	}
	job := syscall.Handle(r0)
	info := jobExtendedLimitInfo{}
	info.basicLimitInformation.limitFlags = jobObjectLimitKillOnJobClose
	r1, _, e1 := procSetInformationJobObject.Call(
		uintptr(job),
		uintptr(jobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r1 == 0 {
		syscall.CloseHandle(job)
		return 0, fmt.Errorf("set job limits: %w", errno(e1))
	}
	return job, nil
}

func assignPIDToJob(job syscall.Handle, pid int) error {
	r0, _, e1 := procOpenProcess.Call(uintptr(syscall.PROCESS_TERMINATE|processSetQuota), 0, uintptr(pid))
	if r0 == 0 {
		return fmt.Errorf("open child: %w", errno(e1))
	}
	process := syscall.Handle(r0)
	defer syscall.CloseHandle(process)
	r1, _, e1 := procAssignProcessToJobObject.Call(uintptr(job), uintptr(process))
	if r1 == 0 {
		return errno(e1)
	}
	return nil
}

func errno(err error) error {
	if err == nil {
		return syscall.EINVAL
	}
	if errno, ok := err.(syscall.Errno); ok && errno == 0 {
		return syscall.EINVAL
	}
	return err
}
