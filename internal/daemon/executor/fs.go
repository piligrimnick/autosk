package executor

import "os"

func mkdirAll(p string, perm os.FileMode) error { return os.MkdirAll(p, perm) }
