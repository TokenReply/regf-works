package grpcweb

import (
	"encoding/binary"
)

// EncodeFrame 编码 gRPC-web 帧：\x00 + 4字节大端长度 + payload
func EncodeFrame(payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = 0x00 // 无压缩
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

// encodeVarint 编码 varint（支持任意大小的值）
func encodeVarint(value uint64) []byte {
	var buf [10]byte
	n := 0
	for value >= 0x80 {
		buf[n] = byte(value) | 0x80
		value >>= 7
		n++
	}
	buf[n] = byte(value)
	n++
	return buf[:n]
}

// encodeTag 编码 protobuf 字段标签
func encodeTag(fieldNumber int, wireType int) []byte {
	return encodeVarint(uint64(fieldNumber<<3 | wireType))
}

// EncodeStringField 编码 protobuf length-delimited string 字段（支持任意长度）
func EncodeStringField(fieldNumber int, value string) []byte {
	data := []byte(value)
	tag := encodeTag(fieldNumber, 2)
	length := encodeVarint(uint64(len(data)))
	buf := make([]byte, 0, len(tag)+len(length)+len(data))
	buf = append(buf, tag...)
	buf = append(buf, length...)
	buf = append(buf, data...)
	return buf
}

// EncodeVarintField 编码 protobuf varint 字段
func EncodeVarintField(fieldNumber int, value int) []byte {
	tag := encodeTag(fieldNumber, 0)
	val := encodeVarint(uint64(value))
	buf := make([]byte, 0, len(tag)+len(val))
	buf = append(buf, tag...)
	buf = append(buf, val...)
	return buf
}

// EncodeBoolField 编码 protobuf bool 字段
func EncodeBoolField(fieldNumber int, value bool) []byte {
	v := 0
	if value {
		v = 1
	}
	return EncodeVarintField(fieldNumber, v)
}

// EncodeBytesField 编码 protobuf bytes/嵌套消息 字段（支持任意长度）
func EncodeBytesField(fieldNumber int, data []byte) []byte {
	tag := encodeTag(fieldNumber, 2)
	length := encodeVarint(uint64(len(data)))
	buf := make([]byte, 0, len(tag)+len(length)+len(data))
	buf = append(buf, tag...)
	buf = append(buf, length...)
	buf = append(buf, data...)
	return buf
}

// EncodeEmailCode 编码发送验证码的 gRPC-web 消息（单字段: email）
func EncodeEmailCode(email string) []byte {
	payload := EncodeStringField(1, email)
	return EncodeFrame(payload)
}

// EncodeVerifyCode 编码验证邮箱码的 gRPC-web 消息（两字段: email + code）
func EncodeVerifyCode(email, code string) []byte {
	payload := append(EncodeStringField(1, email), EncodeStringField(2, code)...)
	return EncodeFrame(payload)
}

// EncodeTosAccepted 编码接受 TOS 的 gRPC-web 消息（固定: field 2 = varint 1）
func EncodeTosAccepted() []byte {
	payload := EncodeVarintField(2, 1)
	return EncodeFrame(payload)
}

// EncodeNsfwSettings 编码 NSFW 设置的 gRPC-web 消息
func EncodeNsfwSettings() []byte {
	// field 1: 嵌套消息 { field 2 = varint 1 }
	inner1 := EncodeVarintField(2, 1)
	field1 := EncodeBytesField(1, inner1)

	// field 2: 嵌套消息 { field 1 = "always_show_nsfw_content" }
	inner2 := EncodeStringField(1, "always_show_nsfw_content")
	field2 := EncodeBytesField(2, inner2)

	payload := append(field1, field2...)
	return EncodeFrame(payload)
}

// EncodeCreateUserAndSession 编码 CreateUserAndSessionRequest (gRPC-web)
//
// Proto 定义（从 accounts.x.ai JS 逆向）:
//
//	message CreateUserAndSessionRequest {
//	  CreateUserRequest create_user_request = 1;
//	  AntiAbuseToken anti_abuse_token = 6;
//	  int32 num_one_time_links = 7;
//	  string email_validation_code = 9;
//	  bool prompt_on_duplicate_email = 10;
//	}
//	message CreateUserRequest {
//	  string given_name = 1;
//	  string family_name = 2;
//	  string email = 3;
//	  string clear_text_password = 5;
//	}
//	message AntiAbuseToken {
//	  string turnstile_token = 1;
//	}
func EncodeCreateUserAndSession(email, givenName, familyName, password, emailValidationCode, turnstileToken string) []byte {
	// 构建 CreateUserRequest（嵌套消息）
	var createUserReq []byte
	createUserReq = append(createUserReq, EncodeStringField(1, givenName)...)
	createUserReq = append(createUserReq, EncodeStringField(2, familyName)...)
	createUserReq = append(createUserReq, EncodeStringField(3, email)...)
	createUserReq = append(createUserReq, EncodeStringField(5, password)...)

	// 构建 CreateUserAndSessionRequest
	var payload []byte
	payload = append(payload, EncodeBytesField(1, createUserReq)...)                   // field 1: create_user_request
	if turnstileToken != "" {                                                          // field 6: anti_abuse_token (可选)
		antiAbuse := EncodeStringField(1, turnstileToken)
		payload = append(payload, EncodeBytesField(6, antiAbuse)...)
	}
	payload = append(payload, EncodeStringField(9, emailValidationCode)...)             // field 9: email_validation_code
	payload = append(payload, EncodeBoolField(10, true)...)                             // field 10: prompt_on_duplicate_email

	return EncodeFrame(payload)
}

// EncodeUnhingedSettings 编码 Unhinged 模式设置 (field 1 = 1, field 2 = 1)
// 参考 grokzhuce nsfw_service.py enable_unhinged 方法
func EncodeUnhingedSettings() []byte {
	payload := append(EncodeVarintField(1, 1), EncodeVarintField(2, 1)...)
	return EncodeFrame(payload)
}
