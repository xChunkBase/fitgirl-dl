package status

import (
	"os"

	"golang.org/x/sys/windows"
)

func init() {
	enableVT = func(f *os.File) bool {
		handle := windows.Handle(f.Fd())
		var mode uint32
		if err := windows.GetConsoleMode(handle, &mode); err != nil {
			return false
		}
		return windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
	}
}
