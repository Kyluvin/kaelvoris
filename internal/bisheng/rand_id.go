// 随机执行 ID（engine.go newExecID 依赖；独立文件避免 crypto 耦合主逻辑）。
package bisheng

import "crypto/rand"

// randUint32 crypto/rand 出的 32 位随机数（newExecID 用）。
func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
