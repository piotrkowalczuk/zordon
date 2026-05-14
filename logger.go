package main

import (
	"fmt"
	"math"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

const (
	keySubsystem = "subsystem"
	keyColor     = "color"
	keyError     = "error"

	ansiReset        = "\x1b[0m"
	ansiLightGreen   = "\x1b[92m"
	ansiLightYellow  = "\x1b[93m"
	ansiLightBlue    = "\x1b[94m"
	ansiLightMagenta = "\x1b[95m"
	ansiLightCyan    = "\x1b[96m"
	ansiLightRed     = "\x1b[91m"
	ansiDim          = "\x1b[2m"
)

var (
	bufferpool = buffer.NewPool()

	servicePalette = []string{
		ansiLightGreen,
		ansiLightYellow,
		ansiLightBlue,
		ansiLightMagenta,
		ansiLightCyan,
	}

	levelColors = map[zapcore.Level]string{
		zapcore.DebugLevel: ansiDim,
		zapcore.InfoLevel:  "",
		zapcore.WarnLevel:  ansiLightYellow,
		zapcore.ErrorLevel: ansiLightRed,
		zapcore.FatalLevel: ansiLightRed,
	}
)

func newLogger() *zap.Logger {
	core := zapcore.NewCore(&zordonEncoder{}, zapcore.Lock(os.Stdout), zapcore.DebugLevel)
	return zap.New(core)
}

type zordonEncoder struct {
	fields []zapcore.Field
}

func (e *zordonEncoder) Clone() zapcore.Encoder {
	return &zordonEncoder{fields: append([]zapcore.Field(nil), e.fields...)}
}

func (e *zordonEncoder) EncodeEntry(ent zapcore.Entry, callFields []zapcore.Field) (*buffer.Buffer, error) {
	buf := bufferpool.Get()

	var color, subsystem string
	rest := make([]zapcore.Field, 0, len(e.fields)+len(callFields))
	collect := func(fs []zapcore.Field) {
		for _, f := range fs {
			switch f.Key {
			case keyColor:
				color = f.String
			case keySubsystem:
				subsystem = f.String
			default:
				rest = append(rest, f)
			}
		}
	}
	collect(e.fields)
	collect(callFields)

	fmt.Fprintf(buf, "[%s] ", ent.Time.Format(time.RFC3339))
	if color != "" {
		buf.AppendString(color)
	}
	fmt.Fprintf(buf, "%-15s", subsystem)
	if color != "" {
		buf.AppendString(ansiReset)
	}
	buf.AppendByte(' ')

	if lc := levelColors[ent.Level]; lc != "" {
		buf.AppendString(lc)
	}
	fmt.Fprintf(buf, "[%-5s]", levelString(ent.Level))
	if lc := levelColors[ent.Level]; lc != "" {
		buf.AppendString(ansiReset)
	}
	buf.AppendString(" - ")
	fmt.Fprintf(buf, "%-60s", ent.Message)

	for _, f := range rest {
		buf.AppendByte(' ')
		buf.AppendString(f.Key)
		buf.AppendByte('=')
		appendFieldValue(buf, f)
	}
	buf.AppendByte('\n')
	return buf, nil
}

func levelString(l zapcore.Level) string {
	switch l {
	case zapcore.DebugLevel:
		return "DEBUG"
	case zapcore.InfoLevel:
		return "INFO"
	case zapcore.WarnLevel:
		return "WARN"
	case zapcore.ErrorLevel:
		return "ERROR"
	case zapcore.FatalLevel:
		return "FATAL"
	}
	return l.CapitalString()
}

func appendFieldValue(buf *buffer.Buffer, f zapcore.Field) {
	switch f.Type {
	case zapcore.StringType:
		buf.AppendString(f.String)
	case zapcore.BoolType:
		buf.AppendBool(f.Integer == 1)
	case zapcore.Int64Type, zapcore.Int32Type, zapcore.Int16Type, zapcore.Int8Type:
		buf.AppendInt(f.Integer)
	case zapcore.Uint64Type, zapcore.Uint32Type, zapcore.Uint16Type, zapcore.Uint8Type:
		buf.AppendUint(uint64(f.Integer))
	case zapcore.Float64Type:
		buf.AppendFloat(math.Float64frombits(uint64(f.Integer)), 64)
	case zapcore.Float32Type:
		buf.AppendFloat(float64(math.Float32frombits(uint32(f.Integer))), 32)
	case zapcore.ErrorType:
		if err, ok := f.Interface.(error); ok {
			buf.AppendString(err.Error())
		}
	case zapcore.ReflectType:
		fmt.Fprintf(buf, "%v", f.Interface)
	default:
		if f.Interface != nil {
			fmt.Fprintf(buf, "%v", f.Interface)
		} else if f.String != "" {
			buf.AppendString(f.String)
		} else {
			buf.AppendInt(f.Integer)
		}
	}
}

func (e *zordonEncoder) AddArray(key string, _ zapcore.ArrayMarshaler) error  { return nil }
func (e *zordonEncoder) AddObject(key string, _ zapcore.ObjectMarshaler) error { return nil }
func (e *zordonEncoder) AddBinary(key string, val []byte)                      { e.fields = append(e.fields, zap.Binary(key, val)) }
func (e *zordonEncoder) AddByteString(key string, val []byte)                  { e.fields = append(e.fields, zap.ByteString(key, val)) }
func (e *zordonEncoder) AddBool(key string, val bool)                          { e.fields = append(e.fields, zap.Bool(key, val)) }
func (e *zordonEncoder) AddComplex128(key string, _ complex128)                {}
func (e *zordonEncoder) AddComplex64(key string, _ complex64)                  {}
func (e *zordonEncoder) AddDuration(key string, val time.Duration)             { e.fields = append(e.fields, zap.Duration(key, val)) }
func (e *zordonEncoder) AddFloat64(key string, val float64)                    { e.fields = append(e.fields, zap.Float64(key, val)) }
func (e *zordonEncoder) AddFloat32(key string, val float32)                    { e.fields = append(e.fields, zap.Float32(key, val)) }
func (e *zordonEncoder) AddInt(key string, val int)                            { e.fields = append(e.fields, zap.Int(key, val)) }
func (e *zordonEncoder) AddInt64(key string, val int64)                        { e.fields = append(e.fields, zap.Int64(key, val)) }
func (e *zordonEncoder) AddInt32(key string, val int32)                        { e.fields = append(e.fields, zap.Int32(key, val)) }
func (e *zordonEncoder) AddInt16(key string, val int16)                        { e.fields = append(e.fields, zap.Int16(key, val)) }
func (e *zordonEncoder) AddInt8(key string, val int8)                          { e.fields = append(e.fields, zap.Int8(key, val)) }
func (e *zordonEncoder) AddString(key, val string)                             { e.fields = append(e.fields, zap.String(key, val)) }
func (e *zordonEncoder) AddTime(key string, val time.Time)                     { e.fields = append(e.fields, zap.Time(key, val)) }
func (e *zordonEncoder) AddUint(key string, val uint)                          { e.fields = append(e.fields, zap.Uint(key, val)) }
func (e *zordonEncoder) AddUint64(key string, val uint64)                      { e.fields = append(e.fields, zap.Uint64(key, val)) }
func (e *zordonEncoder) AddUint32(key string, val uint32)                      { e.fields = append(e.fields, zap.Uint32(key, val)) }
func (e *zordonEncoder) AddUint16(key string, val uint16)                      { e.fields = append(e.fields, zap.Uint16(key, val)) }
func (e *zordonEncoder) AddUint8(key string, val uint8)                        { e.fields = append(e.fields, zap.Uint8(key, val)) }
func (e *zordonEncoder) AddUintptr(key string, val uintptr)                    { e.fields = append(e.fields, zap.Uintptr(key, val)) }
func (e *zordonEncoder) AddReflected(key string, val any) error                { e.fields = append(e.fields, zap.Any(key, val)); return nil }
func (e *zordonEncoder) OpenNamespace(_ string)                                {}
