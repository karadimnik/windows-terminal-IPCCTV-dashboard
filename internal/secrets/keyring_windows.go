//go:build windows

package secrets

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// Windows Credential Manager through advapi32 (no dependency). Entries are GENERIC credentials
// named <prefix><name>, persisted for the local machine, protected by the user's logon (DPAPI
// underneath), which is the protection camera_creds.xml has.
const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
	errorNotFound           = syscall.Errno(1168)
	maxCredBlob             = 5 * 512 // CRED_MAX_CREDENTIAL_BLOB_SIZE
)

var (
	advapi32       = syscall.NewLazyDLL("advapi32.dll")
	procCredWrite  = advapi32.NewProc("CredWriteW")
	procCredRead   = advapi32.NewProc("CredReadW")
	procCredDelete = advapi32.NewProc("CredDeleteW")
	procCredEnum   = advapi32.NewProc("CredEnumerateW")
	procCredFree   = advapi32.NewProc("CredFree")
)

// credential is CREDENTIALW.
type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

type keyringProvider struct{ prefix string }

// newKeyring checks that the Credential Manager answers in this session (a read of a random
// absent target must say "not found"; a service or remote session without a profile does not).
func newKeyring(prefix string) (Provider, error) {
	k := keyringProvider{prefix}
	probe := "holocron-probe/" + randHex(8)
	_, err := readCred(probe)
	if err != nil && err != errorNotFound {
		return nil, fmt.Errorf("%w: %v", errKeyringUnusable, err)
	}
	return k, nil
}

func (keyringProvider) Backend() string { return BackendKeyring }

func (k keyringProvider) target(name string) string { return k.prefix + name }

func readCred(target string) ([]byte, error) {
	t, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return nil, err
	}
	var c *credential
	r, _, e := procCredRead.Call(uintptr(unsafe.Pointer(t)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&c)))
	if r == 0 {
		return nil, e
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(c)))
	out := make([]byte, c.CredentialBlobSize)
	if c.CredentialBlobSize > 0 {
		copy(out, unsafe.Slice(c.CredentialBlob, c.CredentialBlobSize))
	}
	return out, nil
}

func (k keyringProvider) Get(name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	b, err := readCred(k.target(name))
	if err == errorNotFound {
		return "", notFound(name)
	}
	if err != nil {
		return "", fmt.Errorf("secrets: credential manager read: %w", err)
	}
	s := string(b)
	for i := range b {
		b[i] = 0
	}
	return s, nil
}

func (k keyringProvider) Put(name, value string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkValue(value); err != nil {
		return err
	}
	if len(value) > maxCredBlob {
		return fmt.Errorf("%w: the Credential Manager holds at most %d bytes", ErrInvalidValue, maxCredBlob)
	}
	t, err := syscall.UTF16PtrFromString(k.target(name))
	if err != nil {
		return err
	}
	u, _ := syscall.UTF16PtrFromString("holocron")
	blob := []byte(value)
	c := credential{
		Type:               credTypeGeneric,
		TargetName:         t,
		CredentialBlobSize: uint32(len(blob)),
		CredentialBlob:     &blob[0],
		Persist:            credPersistLocalMachine,
		UserName:           u,
	}
	r, _, e := procCredWrite.Call(uintptr(unsafe.Pointer(&c)), 0)
	for i := range blob {
		blob[i] = 0
	}
	if r == 0 {
		return fmt.Errorf("secrets: credential manager write: %w", e)
	}
	return nil
}

func (k keyringProvider) Delete(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	t, err := syscall.UTF16PtrFromString(k.target(name))
	if err != nil {
		return err
	}
	r, _, e := procCredDelete.Call(uintptr(unsafe.Pointer(t)), credTypeGeneric, 0)
	if r == 0 {
		if e == errorNotFound {
			return notFound(name)
		}
		return fmt.Errorf("secrets: credential manager delete: %w", e)
	}
	return nil
}

// List enumerates only <prefix>* and returns the names; blobs are never copied.
func (k keyringProvider) List() ([]string, error) {
	filter, err := syscall.UTF16PtrFromString(k.prefix + "*")
	if err != nil {
		return nil, err
	}
	var count uint32
	var creds **credential
	r, _, e := procCredEnum.Call(uintptr(unsafe.Pointer(filter)), 0, uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&creds)))
	if r == 0 {
		if e == errorNotFound {
			return []string{}, nil
		}
		return nil, fmt.Errorf("secrets: credential manager enumerate: %w", e)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(creds)))
	names := []string{}
	for _, c := range unsafe.Slice(creds, count) {
		if c.Type != credTypeGeneric {
			continue
		}
		full := syscall.UTF16ToString(unsafe.Slice(c.TargetName, utf16Len(c.TargetName)))
		if n := strings.TrimPrefix(full, k.prefix); strings.HasPrefix(full, k.prefix) && ValidName(n) {
			names = append(names, n)
		}
	}
	return sorted(names), nil
}

func utf16Len(p *uint16) int {
	n := 0
	for *(*uint16)(unsafe.Add(unsafe.Pointer(p), 2*n)) != 0 {
		n++
	}
	return n
}
