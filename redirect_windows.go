package main

import "os"

func redirectStderr(f *os.File) error {
	// Windows 不支持 Dup2，直接跳过
	return nil
}
