package consensus

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// 命令的线上编码：用 stdlib 的长度前缀格式，刻意不引入 msgpack/cbor 之类。
//
// 为什么命令信封不需要 msgpack：Command 对 consensus 而言只有 Kind（字符串）、
// InstanceID（字符串）、Payload（不透明字节，由 runtime 自己解释）。信封是
// 定长头 + 变长尾，stdlib binary 就够，没必要为一个信封再吃一个序列化依赖。
// Payload 内部要不要结构化，是 runtime 的事（它可以用任何它喜欢的编码）。
//
// 线格式（大端）：
//
//	[0]      版本 = cmdEncodingVersion
//	[1..4]   kindLen(uint32)
//	[..]     kind bytes
//	[..+4]   instanceIDLen(uint32)
//	[..]     instanceID bytes
//	[..+4]   payloadLen(uint32)
//	[..]     payload bytes
const cmdEncodingVersion = 1

var (
	errTruncated   = errors.New("consensus: truncated command")
	errBadVersion  = errors.New("consensus: unsupported command encoding version")
	errLengthField = errors.New("consensus: inconsistent command length field")
)

// encodeCommand 把 Command 编码为可写入 raft 日志的字节。
//
// 确定性说明：编码是纯函数（同输入→同输出），但日志字节本身的一致性由 raft
// 保证（多数派复制同一批字节）；这里的确定性要求是「解码能还原」，不涉及随机。
func encodeCommand(c Command) []byte {
	kindLen := len(c.Kind)
	idLen := len(c.InstanceID)
	payloadLen := len(c.Payload)

	buf := make([]byte, 0, 1+4+kindLen+4+idLen+4+payloadLen)
	buf = append(buf, cmdEncodingVersion)
	buf = binary.BigEndian.AppendUint32(buf, uint32(kindLen))
	buf = append(buf, c.Kind...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(idLen))
	buf = append(buf, c.InstanceID...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(payloadLen))
	buf = append(buf, c.Payload...)
	return buf
}

// decodeCommand 把 raft 日志字节还原为 Command。
//
// 严格的边界校验：任何长度字段越界、剩余字节不足都判为坏日志（errTruncated /
// errLengthField），由调用方记为不可恢复错误（坏日志不能跳过，跳过即分歧）。
func decodeCommand(b []byte) (Command, error) {
	if len(b) < 1 {
		return Command{}, errTruncated
	}
	if b[0] != cmdEncodingVersion {
		return Command{}, fmt.Errorf("%w: got %d", errBadVersion, b[0])
	}
	pos := 1

	readLen := func() (uint32, error) {
		if pos+4 > len(b) {
			return 0, errTruncated
		}
		v := binary.BigEndian.Uint32(b[pos : pos+4])
		pos += 4
		return v, nil
	}
	readBytes := func(n uint32) ([]byte, error) {
		if uint64(n) > uint64(len(b)-pos) {
			return nil, errLengthField
		}
		start := pos
		pos += int(n)
		return b[start:pos], nil
	}

	kindLen, err := readLen()
	if err != nil {
		return Command{}, err
	}
	kindBytes, err := readBytes(kindLen)
	if err != nil {
		return Command{}, err
	}
	idLen, err := readLen()
	if err != nil {
		return Command{}, err
	}
	idBytes, err := readBytes(idLen)
	if err != nil {
		return Command{}, err
	}
	payloadLen, err := readLen()
	if err != nil {
		return Command{}, err
	}
	payload, err := readBytes(payloadLen)
	if err != nil {
		return Command{}, err
	}
	// 必须消费完：尾部还有多余字节说明编码与解码不一致，判坏日志。
	if pos != len(b) {
		return Command{}, errLengthField
	}

	return Command{
		Kind:       Kind(kindBytes),
		InstanceID: string(idBytes),
		Payload:    payload,
	}, nil
}
