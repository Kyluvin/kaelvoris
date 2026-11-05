package remote

import "crypto/rand"

// randRead crypto/rand 包装（避免与 syscall/rand 名冲突）。
func randRead(b []byte) (int, error) {
	return rand.Read(b)
}
