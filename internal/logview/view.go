package logview

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"time"
)

// Run prints the last part of path then follows new lines (tail -f).
// Blocks until stdin closes or process is killed.
func Run(path string) error {
	fmt.Println("======== hellogrok 日志 ========")
	fmt.Println(path)
	fmt.Println("（实时刷新，关闭本窗口即可退出）")
	fmt.Println("================================")
	fmt.Println()

	// ensure exists
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_ = f.Close()

	// print last ~64KiB
	if err := printTail(path, 64*1024); err != nil {
		return err
	}

	// follow
	var offset int64
	if st, err := os.Stat(path); err == nil {
		offset = st.Size()
	}
	for {
		st, err := os.Stat(path)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		size := st.Size()
		if size < offset {
			// truncated
			offset = 0
			fmt.Print("\n--- 日志已截断，重新读取 ---\n")
		}
		if size > offset {
			if err := printFrom(path, offset, size); err != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			offset = size
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func printTail(path string, max int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	start := int64(0)
	if st.Size() > max {
		start = st.Size() - max
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return err
		}
		// skip partial first line
		br := bufio.NewReader(f)
		_, _ = br.ReadString('\n')
		_, _ = io.Copy(os.Stdout, br)
		return nil
	}
	_, err = io.Copy(os.Stdout, f)
	return err
}

func printFrom(path string, from, to int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return err
	}
	_, err = io.CopyN(os.Stdout, f, to-from)
	return err
}
