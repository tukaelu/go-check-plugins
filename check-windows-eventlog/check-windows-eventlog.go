// +build windows

package main

import (
	"crypto/md5"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"github.com/jessevdk/go-flags"
	"github.com/mackerelio/checkers"
	"github.com/mackerelio/go-check-plugins/check-windows-eventlog/internal/eventlog"
)

const (
	errorInvalidParameter = syscall.Errno(87)
)

var (
	rid1 = regexp.MustCompile(`^([0-9]+)$`)
	rid2 = regexp.MustCompile(`^([0-9]+)-([0-9]+)$`)
)

type idRange struct {
	hi   uint32
	lo   uint32
	bang bool
}

type logOpts struct {
	Log            string `long:"log" description:"Event Names (comma separated)"`
	Type           string `long:"type" description:"Event Types (comma separated)"`
	SourcePattern  string `long:"source-pattern" description:"Event Source (regexp pattern)"`
	SourceExclude  string `long:"source-exclude" description:"Event Source excluded (regexp pattern)"`
	MessagePattern string `long:"message-pattern" description:"Message Pattern (regexp pattern)"`
	MessageExclude string `long:"message-exclude" description:"Message Pattern excluded (regexp pattern)"`
	EventID        string `long:"event-id" description:"Event IDs (separated by comma)"`
	WarnOver       int64  `short:"w" long:"warning-over" description:"Trigger a warning if matched lines is over a number"`
	CritOver       int64  `short:"c" long:"critical-over" description:"Trigger a critical if matched lines is over a number"`
	ReturnContent  bool   `short:"r" long:"return" description:"Return matched line"`
	StateDir       string `short:"s" long:"state-dir" value-name:"DIR" description:"Dir to keep state files under"`
	NoState        bool   `long:"no-state" description:"Don't use state file and read whole logs"`
	FailFirst      bool   `long:"fail-first" description:"Count errors on first seek"`
	Verbose        bool   `long:"verbose" description:"Verbose output"`

	logList        []string
	typeList       []string
	idRangeList    []idRange
	sourcePattern  *regexp.Regexp
	sourceExclude  *regexp.Regexp
	messagePattern *regexp.Regexp
	messageExclude *regexp.Regexp
	origArgs       []string
}

func stringList(s string) []string {
	l := strings.Split(s, ",")
	if len(l) == 0 || l[0] == "" {
		return []string{}
	}
	return l
}

func idRangeList(s string) ([]idRange, error) {
	var idrl []idRange
	var id1, id2 uint64
	var err error
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		bang := strings.HasPrefix(t, "!")
		if bang {
			t = t[1:]
		}
		if m1 := rid1.FindAllStringSubmatch(t, -1); len(m1) > 0 {
			id1, err = strconv.ParseUint(m1[0][1], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid id list: %v", err)
			}
			id2 = id1
		} else if m2 := rid2.FindAllStringSubmatch(t, -1); len(m2) > 0 {
			id1, err = strconv.ParseUint(m2[0][1], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid id list: %v", err)
			}
			id2, err = strconv.ParseUint(m2[0][2], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid id list: %v", err)
			}
			if id1 > id2 {
				id1, id2 = id2, id1
			}
		} else {
			return nil, fmt.Errorf("invalid id list")
		}
		idrl = append(idrl, idRange{lo: uint32(id1), hi: uint32(id2), bang: bang})
	}
	return idrl, nil
}

func (opts *logOpts) prepare() error {
	opts.logList = stringList(opts.Log)
	if len(opts.logList) == 0 || opts.logList[0] == "" {
		opts.logList = []string{"Application"}
	}
	opts.typeList = stringList(opts.Type)

	var err error

	if opts.EventID != "" {
		opts.idRangeList, err = idRangeList(opts.EventID)
		if err != nil {
			return err
		}
	}
	if opts.SourcePattern != "" {
		opts.sourcePattern, err = regexp.Compile(opts.SourcePattern)
		if err != nil {
			return err
		}
	}
	if opts.SourceExclude != "" {
		opts.sourceExclude, err = regexp.Compile(opts.SourceExclude)
		if err != nil {
			return err
		}
	}
	if opts.MessagePattern != "" {
		opts.messagePattern, err = regexp.Compile(opts.MessagePattern)
		if err != nil {
			return err
		}
	}
	if opts.MessageExclude != "" {
		opts.messageExclude, err = regexp.Compile(opts.MessageExclude)
		if err != nil {
			return err
		}
	}
	return nil
}

func main() {
	ckr := run(os.Args[1:])
	ckr.Name = "Event Log"
	ckr.Exit()
}

func parseArgs(args []string) (*logOpts, error) {
	origArgs := make([]string, len(args))
	copy(origArgs, args)
	opts := &logOpts{}
	_, err := flags.ParseArgs(opts, args)
	if opts.StateDir == "" {
		workdir := os.Getenv("MACKEREL_PLUGIN_WORKDIR")
		if workdir == "" {
			workdir = os.TempDir()
		}
		opts.StateDir = filepath.Join(workdir, "check-windows-eventlog")
	}
	opts.origArgs = origArgs
	return opts, err
}

func run(args []string) *checkers.Checker {
	opts, err := parseArgs(args)
	if err != nil {
		os.Exit(1)
	}

	err = opts.prepare()
	if err != nil {
		return checkers.Unknown(err.Error())
	}

	checkSt := checkers.OK
	warnNum := int64(0)
	critNum := int64(0)
	errorOverall := ""

	for _, lt := range opts.logList {
		w, c, errLines, err := opts.searchLog(lt)
		if err != nil {
			return checkers.Unknown(err.Error())
		}
		warnNum += w
		critNum += c
		if opts.ReturnContent {
			errorOverall += errLines
		}
	}
	msg := fmt.Sprintf("%d warnings, %d criticals.", warnNum, critNum)
	if errorOverall != "" {
		msg += "\n" + errorOverall
	}
	if warnNum > opts.WarnOver {
		checkSt = checkers.WARNING
	}
	if critNum > opts.CritOver {
		checkSt = checkers.CRITICAL
	}
	return checkers.NewChecker(checkSt, msg)
}

func bytesToString(b []byte) (string, uint32) {
	var i int
	s := make([]uint16, len(b)/2)
	for i = range s {
		s[i] = uint16(b[i*2]) + uint16(b[(i*2)+1])<<8
		if s[i] == 0 {
			s = s[0:i]
			break
		}
	}
	return string(utf16.Decode(s)), uint32(i * 2)
}

func getResourceMessage(providerName, sourceName string, eventID uint32, argsptr uintptr) (string, error) {
	regkey := fmt.Sprintf(
		"SYSTEM\\CurrentControlSet\\Services\\EventLog\\%s\\%s",
		providerName, sourceName)
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, regkey, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer key.Close()

	val, _, err := key.GetStringValue("EventMessageFile")
	if err != nil {
		return "", err
	}
	val, err = registry.ExpandString(val)
	if err != nil {
		return "", err
	}

	handle, err := eventlog.LoadLibraryEx(syscall.StringToUTF16Ptr(val), 0,
		eventlog.DONT_RESOLVE_DLL_REFERENCES|eventlog.LOAD_LIBRARY_AS_DATAFILE)
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(handle)

	msgbuf := make([]byte, 1<<16)
	numChars, err := eventlog.FormatMessage(
		syscall.FORMAT_MESSAGE_FROM_SYSTEM|
			syscall.FORMAT_MESSAGE_FROM_HMODULE|
			syscall.FORMAT_MESSAGE_ARGUMENT_ARRAY,
		handle,
		eventID,
		0,
		&msgbuf[0],
		uint32(len(msgbuf)),
		argsptr)
	if err != nil {
		return "", err
	}
	message, _ := bytesToString(msgbuf[:numChars*2])
	message = strings.Replace(message, "\r", "", -1)
	message = strings.TrimSuffix(message, "\n")
	return message, nil
}

func (opts *logOpts) searchLog(logName string) (warnNum, critNum int64, errLines string, err error) {
	stateFile := opts.getStateFile(logName)
	recordNumber := uint32(0)
	if !opts.NoState {
		s, err := getLastOffset(stateFile)
		if err != nil && !os.IsNotExist(err) {
			return 0, 0, "", err
		}
		recordNumber = uint32(s)
	}

	ptr := syscall.StringToUTF16Ptr(logName)
	h, err := eventlog.OpenEventLog(nil, ptr)
	if err != nil {
		log.Fatal(err)
	}
	defer eventlog.CloseEventLog(h)

	var num, oldnum, lastNumber uint32

	eventlog.GetNumberOfEventLogRecords(h, &num)
	if err != nil {
		log.Fatal(err)
	}
	eventlog.GetOldestEventLogRecord(h, &oldnum)
	if err != nil {
		log.Fatal(err)
	}

	if recordNumber == 0 {
		if !opts.NoState && !opts.FailFirst {
			err = writeLastOffset(stateFile, int64(oldnum+num-1))
			return 0, 0, "", err
		}
	}

	if oldnum <= recordNumber {
		if recordNumber == oldnum+num-1 {
			return 0, 0, "", nil
		}
		lastNumber = recordNumber
		recordNumber++
	} else {
		recordNumber = oldnum
	}

	size := uint32(1)
	buf := []byte{0}

	var readBytes uint32
	var nextSize uint32

loop_events:
	for i := recordNumber; i < oldnum+num; i++ {
		flags := eventlog.EVENTLOG_FORWARDS_READ | eventlog.EVENTLOG_SEEK_READ
		if i == 0 {
			flags = eventlog.EVENTLOG_FORWARDS_READ | eventlog.EVENTLOG_SEQUENTIAL_READ
		}

		err = eventlog.ReadEventLog(
			h,
			flags,
			i,
			&buf[0],
			size,
			&readBytes,
			&nextSize)
		if err != nil {
			if err != syscall.ERROR_INSUFFICIENT_BUFFER {
				if err != errorInvalidParameter {
					return 0, 0, "", err
				}
				break
			}
			buf = make([]byte, nextSize)
			size = nextSize
			err = eventlog.ReadEventLog(
				h,
				flags,
				i,
				&buf[0],
				size,
				&readBytes,
				&nextSize)
			if err != nil {
				log.Printf("eventlog.ReadEventLog: %v", err)
				break
			}
		}

		r := *(*eventlog.EVENTLOGRECORD)(unsafe.Pointer(&buf[0]))
		if opts.Verbose {
			log.Printf("RecordNumber=%v", r.RecordNumber)
			log.Printf("TimeGenerated=%v", time.Unix(int64(r.TimeGenerated), 0).String())
			log.Printf("TimeWritten=%v", time.Unix(int64(r.TimeWritten), 0).String())
			log.Printf("EventID=%v", r.EventID)
		}
		lastNumber = r.RecordNumber

		if opts.idRangeList != nil {
			// even code takes last 4 bytes
			eventID := r.EventID & 0x0000FFFF
			found := false
			exact := false
			for _, idr := range opts.idRangeList {
				if !idr.bang {
					exact = true
					if idr.lo <= eventID && eventID <= idr.hi {
						found = true
					}
				} else {
					if idr.lo <= eventID && eventID <= idr.hi {
						found = false
					} else if !exact {
						found = true
					}
				}
			}
			if !found {
				continue loop_events
			}
		}

		tn := eventlog.EventType(r.EventType).String()
		if opts.Verbose {
			log.Printf("EventType=%v", tn)
		}
		if len(opts.typeList) > 0 {
			found := false
			for _, typ := range opts.typeList {
				if typ == tn {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		sourceName, sourceNameOff := bytesToString(buf[unsafe.Sizeof(eventlog.EVENTLOGRECORD{}):])
		computerName, _ := bytesToString(buf[unsafe.Sizeof(eventlog.EVENTLOGRECORD{})+uintptr(sourceNameOff+2):])
		if opts.Verbose {
			log.Printf("SourceName=%v", sourceName)
			log.Printf("ComputerName=%v", computerName)
		}

		// match source if pattern provied
		if opts.sourcePattern != nil {
			if !opts.sourcePattern.MatchString(sourceName) {
				continue
			}
		}
		// exclude-match source if pattern provied
		if opts.sourceExclude != nil {
			if opts.sourceExclude.MatchString(sourceName) {
				continue
			}
		}

		off := uint32(0)
		args := make([]*byte, uintptr(r.NumStrings)*unsafe.Sizeof((*uint16)(nil)))
		for n := 0; n < int(r.NumStrings); n++ {
			args[n] = &buf[r.StringOffset+off]
			_, boff := bytesToString(buf[r.StringOffset+off:])
			off += boff + 2
		}

		var argsptr uintptr
		if r.NumStrings > 0 {
			argsptr = uintptr(unsafe.Pointer(&args[0]))
		}
		message, _ := getResourceMessage(logName, sourceName, r.EventID, argsptr)
		if opts.Verbose {
			log.Printf("Message=%v", message)
		}

		// match message if pattern provied
		if opts.messagePattern != nil {
			if !opts.messagePattern.MatchString(message) {
				continue
			}
		}
		// exclude-match message if pattern provied
		if opts.messageExclude != nil {
			if opts.messageExclude.MatchString(message) {
				continue
			}
		}

		if opts.ReturnContent {
			if message == "" {
				message = "[mackerel-agent] No message resource found. Please make sure the event log occured on the server."
			}
			errLines += sourceName + ":" + strings.Replace(message, "\n", "", -1) + "\n"
		}
		switch tn {
		case "Error":
			critNum++
		case "Audit Failure":
			critNum++
		case "Warning":
			warnNum++
		}
	}

	if !opts.NoState {
		err = writeLastOffset(stateFile, int64(lastNumber))
		if err != nil {
			log.Printf("writeLastOffset failed: %s\n", err.Error())
		}
	}

	if recordNumber == 0 && !opts.FailFirst {
		return 0, 0, "", nil
	}
	return warnNum, critNum, errLines, nil
}

var stateRe = regexp.MustCompile(`^([A-Z]):[/\\]`)

func (opts *logOpts) getStateFile(logName string) string {
	return filepath.Join(
		opts.StateDir,
		fmt.Sprintf(
			"%s-%x",
			stateRe.ReplaceAllString(logName, `$1`+string(filepath.Separator)),
			md5.Sum([]byte(strings.Join(opts.origArgs, " "))),
		),
	)
}

func getLastOffset(f string) (int64, error) {
	_, err := os.Stat(f)
	if err != nil {
		return 0, err
	}
	b, err := ioutil.ReadFile(f)
	if err != nil {
		return 0, err
	}
	i, err := strconv.ParseInt(strings.Trim(string(b), " \r\n"), 10, 64)
	if err != nil {
		return 0, err
	}
	return i, nil
}

func writeLastOffset(f string, num int64) error {
	err := os.MkdirAll(filepath.Dir(f), 0755)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(f, []byte(fmt.Sprintf("%d", num)), 0644)
}
