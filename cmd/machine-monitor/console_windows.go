package main

import "syscall"

// detachConsole frees the console the logon task gave this process, closing its window.
func detachConsole() {
	syscall.NewLazyDLL("kernel32.dll").NewProc("FreeConsole").Call()
}
