package sl651

// CRC16 计算 SL 651 的校验码。
//
// 算法（规约附录 H.3.2 原文逐步描述，这里逐条对应）：
//
//	第一步 置 16 位 CRC 寄存器为 0FFFFH；
//	第二步 取数据字节与寄存器低字节异或，结果存回寄存器；
//	第三步 寄存器右移一位，最高位补 0，移出位待检测；
//	第四步 移出位为 0 则重复第三步，为 1 则与 A001H 异或；
//	第五步 重复第三、四步 8 次，处理完一个字节；
//	第六步 对每个字节重复第二到第五步。
//
// **规约在第六步之后就结束了**："最终 CRC 寄存器的值就是 CRC 的值"——没有
// 常见的"结果取反"这一步。这一点与仓库里其它表计协议（DL/T 645、CJ/T 188
// 用的是 CRC-16/X-25，初值 FFFF 且结果取反）不同，实现时最容易照着抄错。
// 金向量的 C3AE / 20FA 只在「不取反」时成立，取反会得到 3C51 / DF05。
//
// 等价的标准名称是 **CRC-16/MODBUS**（poly 8005 反射为 A001、init FFFF、
// refin=refout=true、**xorout 0000**）。刻意不把这个函数叫 CRC16X25：
// X-25 与 MODBUS 只差最后一步取反，而这一步正好是「值看似正常、实际全错」
// 的那类错误——名字里带 X25 会诱导下一个人照着 X-25 去"修正"。
//
// 还有一处与 MODBUS 不同：MODBUS 传输时低字节在前，SL 651 要求**高位字节
// 在前**。传输序与计算无关，见 AppendCRC。
func CRC16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 == 1 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// AppendCRC 把校验码追加到 buf 末尾（高位字节在前），返回新切片。
func AppendCRC(buf []byte) []byte {
	crc := CRC16(buf)
	return append(buf, byte(crc>>8), byte(crc&0xFF))
}

// CheckCRC 校验 buf 末尾 2 字节（高位在前）是否是前面字节的校验码。
func CheckCRC(buf []byte) bool {
	if len(buf) < crcLen {
		return false
	}
	want := uint16(buf[len(buf)-2])<<8 | uint16(buf[len(buf)-1])
	return want == CRC16(buf[:len(buf)-2])
}
