//go:build windows
// +build windows

// Package pageant provides native Go support for using PuTTY Pageant as an
// SSH agent with the golang.org/x/crypto/ssh/agent package.
// Based loosely on the Java JNA package jsch-agent-proxy-pageant.
// Based on https://github.com/kbolino/pageant of Kristian Bolino
package pageant

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	agentCopydataID = 0x804e50ba
	agentMaxMsglen  = 8192
	noError         = syscall.Errno(0)
	wmCopyData      = 0x004a
)

var (
	pageantWindowName = utf16Ptr("Pageant")
	user32            = windows.NewLazySystemDLL("user32.dll")
	findWindow        = user32.NewProc("FindWindowW")
	sendMessage       = user32.NewProc("SendMessageW")
)

var connUniqueID atomic.Uint64

// Conn is a shared-memory connection to Pageant.
// Conn implements net.Reader, net.Writer, and net.Closer.
// It is not safe to use Conn in multiple concurrent goroutines.
type Conn struct {
	window     windows.Handle
	sharedFile windows.Handle
	sharedMem  uintptr
	mapName    string
	data       chan []byte
	buf        []byte
	eof        bool
	sync.Mutex
}

// NewConn creates a new connection to Pageant or to ssh-agent.exe of OpenSSH_for_Windows
// Ensure Close gets called on the returned Conn when it is no longer needed.
func NewConn() (net.Conn, error) {
	const (
		PIPE        = `\\.\pipe\`
		sshAuthPipe = "openssh-ssh-agent"
		sshAuthSock = "SSH_AUTH_SOCK"
	)
	_, err := PageantWindow()
	if err == nil {
		return NewPageantConn()
	}

	sockPath := os.Getenv("SSH_AUTH_SOCK")
	if sockPath == "" {
		sockPath = sshAuthPipe
	}
	if !strings.HasPrefix(sockPath, PIPE) {
		sockPath = PIPE + sockPath
	}
	return winio.DialPipe(sockPath, nil)
}

// PageantAvailable returns pageant available or not.
func PageantAvailable() bool {
	if _, err := PageantWindow(); err == nil {
		return true
	}
	return false
}

// NewPageantConn returns new connection to pageant.
func NewPageantConn() (net.Conn, error) {
	if !PageantAvailable() {
		return nil, fmt.Errorf("pageant is not available")
	}
	c := &Conn{data: make(chan []byte, 10)}
	if err := c.establishConn(); err != nil {
		return nil, fmt.Errorf("failed to connect to Pageant: %s", err)
	}
	return c, nil
}

// for net.Conn
func (c *Conn) LocalAddr() net.Addr {
	return nil
}
func (c *Conn) RemoteAddr() net.Addr {
	return nil
}
func (c *Conn) SetDeadline(_ time.Time) error {
	return nil
}
func (c *Conn) SetReadDeadline(_ time.Time) error {
	return nil
}
func (c *Conn) SetWriteDeadline(_ time.Time) error {
	return nil
}

// Close frees resources used by Conn.
func (c *Conn) Close() error {
	if c.sharedMem == 0 {
		return nil
	}

	c.Lock()
	defer c.Unlock()

	close(c.data)

	errUnmap := windows.UnmapViewOfFile(c.sharedMem)
	errClose := windows.CloseHandle(c.sharedFile)
	if errUnmap != nil {
		return errUnmap
	} else if errClose != nil {
		return errClose
	}
	c.sharedMem = 0
	c.sharedFile = windows.InvalidHandle
	return nil
}

func (c *Conn) Read(p []byte) (n int, err error) {
	if c.eof {
		return 0, io.EOF
	}

	if c.sharedMem == 0 {
		return 0, fmt.Errorf("not connected to Pageant")
	}

	if len(c.buf) == 0 {
		var ok bool
		c.buf, ok = <-c.data
		if !ok {
			c.eof = true
			return 0, io.EOF
		}
	}

	n = copy(p, c.buf)
	c.buf = c.buf[n:]
	return
}

// close, establishConn, sendMessage
func (c *Conn) Write(p []byte) (n int, err error) {
	if len(p) > agentMaxMsglen {
		return 0, fmt.Errorf("size of request message (%d) exceeds max length (%d)", len(p), agentMaxMsglen)
	} else if len(p) == 0 {
		return 0, fmt.Errorf("message to send is empty")
	}

	c.Lock()
	defer c.Unlock()

	if c.sharedMem == 0 {
		return 0, fmt.Errorf("not connected to Pageant")
	}

	dst := toSlice(c.sharedMem, len(p))
	copy(dst, p)
	data := make([]byte, len(c.mapName)+1)
	copy(data, c.mapName)
	result, err := c.sendMessage(data)
	if result == 0 {
		if err != nil {
			return 0, fmt.Errorf("failed to send request to Pageant: %s", err)
		} else {
			return 0, fmt.Errorf("request refused by Pageant")
		}
	}
	messageSize := binary.BigEndian.Uint32(toSlice(c.sharedMem, 4))
	if messageSize > agentMaxMsglen-4 {
		return 0, fmt.Errorf("size of response message (%d) exceeds max length (%d)", messageSize+4, agentMaxMsglen)
	}

	buf := make([]byte, 4+int(messageSize))
	src := toSlice(c.sharedMem, 4+int(messageSize))
	copy(buf, src)
	c.data <- buf

	return len(p), nil
}

// used in establishConn and NewConn
func PageantWindow() (window uintptr, err error) {
	window, _, err = findWindow.Call(
		uintptr(unsafe.Pointer(pageantWindowName)),
		uintptr(unsafe.Pointer(pageantWindowName)),
	)
	if window == 0 {
		if err != nil && err != noError {
			err = fmt.Errorf("cannot find Pageant window: %s", err)
		} else {
			err = fmt.Errorf("cannot find Pageant window, ensure Pageant is running")
		}
	} else {
		err = nil
	}
	return
}

// establishConn creates a new connection to Pageant.
func (c *Conn) establishConn() error {
	window, err := PageantWindow()
	if err != nil {
		return err
	}

	mapName := fmt.Sprintf("PageantRequest_%x_%x", os.Getpid(), connUniqueID.Add(1))
	mapNameUTF16 := utf16Ptr(mapName)
	sharedFile, err := windows.CreateFileMapping(
		windows.InvalidHandle,
		nil,
		windows.PAGE_READWRITE,
		0,
		agentMaxMsglen,
		mapNameUTF16,
	)
	if err != nil {
		return fmt.Errorf("failed to create shared file: %s", err)
	}
	sharedMem, err := windows.MapViewOfFile(
		sharedFile,
		windows.FILE_MAP_WRITE,
		0,
		0,
		0,
	)
	if err != nil {
		return fmt.Errorf("failed to map file into shared memory: %s", err)
	}
	c.window = windows.Handle(window)
	c.sharedFile = sharedFile
	c.sharedMem = sharedMem
	c.mapName = mapName
	return nil
}

// sendMessage invokes user32.SendMessage to alert Pageant that data
// is available for it to read.
func (c *Conn) sendMessage(data []byte) (uintptr, error) {
	cds := copyData{
		dwData: agentCopydataID,
		cbData: uintptr(len(data)),
		lpData: uintptr(unsafe.Pointer(&data[0])),
	}
	result, _, err := sendMessage.Call(
		uintptr(c.window),
		wmCopyData,
		0,
		uintptr(unsafe.Pointer(&cds)),
	)
	if err == noError {
		return result, nil
	}
	return result, err
}

// copyData is equivalent to COPYDATASTRUCT.
// Unlike Java, Go has a native type that matches the bit width of the
// platform, so there is no need for separate 32-bit and 64-bit versions.
// Curiously, the MSDN definition of COPYDATASTRUCT says dwData is ULONG_PTR
// and cbData is DWORD, which seems to be backwards.
type copyData struct {
	dwData uint32
	cbData uintptr
	lpData uintptr
}

// minInt returns the lesser of x and y.
func minInt(x, y int) int {
	if x < y {
		return x
	} else {
		return y
	}
}

// toSlice creates a fake slice header that allows copying to/from the block
// of memory from addr to addr+size.
func toSlice(addr uintptr, size int) []byte {
	header := reflect.SliceHeader{
		Len:  size,
		Cap:  size,
		Data: addr,
	}
	return *(*[]byte)(unsafe.Pointer(&header))
}

// utf16Ptr converts a static string not containing any zero bytes to a
// sequence of UTF-16 code units, represented as a pointer to the first one.
func utf16Ptr(s string) *uint16 {
	result, err := windows.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return result
}
