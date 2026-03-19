package helper

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func InitLogger(dev_mode bool) (logger *zap.Logger, log *zap.SugaredLogger) {
	var err error

	if dev_mode {
		encoderConfig := zapcore.EncoderConfig{
			MessageKey:     "msg",
			LevelKey:       "level",
			TimeKey:        "", // Omit time in development mode
			NameKey:        "logger",
			CallerKey:      "caller", // This is where the caller (method/function) info goes
			EncodeLevel:    zapcore.CapitalColorLevelEncoder,
			EncodeDuration: zapcore.StringDurationEncoder,
			EncodeCaller:   customMethodNameEncoder, // Use our custom encoder here
		}
		core := zapcore.NewCore(
			zapcore.NewConsoleEncoder(encoderConfig),
			os.Stdout,
			zap.DebugLevel,
		)
		// Always need zap.AddCaller() to capture caller info from the runtime
		logger = zap.New(core, zap.AddCaller())
	} else {
		logger, err = zap.NewProduction()
	}

	if err != nil {
		panic(fmt.Errorf("failed to initialize logger: %w", err))
	}

	log = logger.Sugar()

	return logger, log
}

const (
	LoggerKey = "logger"
)

// InjectLoggerInContextMiddleware injects the Zap logger into the Gin context.
func InjectLoggerInContextMiddleware(logger *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(LoggerKey, logger)
		c.Next()
	}
}

func customMethodNameEncoder(caller zapcore.EntryCaller, enc zapcore.PrimitiveArrayEncoder) {
	functionName := caller.Function

	// Extract just the bare function/method name (e.g., "myMethod" from "main.myMethod")
	lastDot := strings.LastIndexByte(functionName, '.')
	bareFunctionName := functionName
	if lastDot != -1 {
		bareFunctionName = functionName[lastDot+1:]
	}

	formattedCaller := fmt.Sprintf("(%s:%d) %s", filepath.Base(caller.File), caller.Line, bareFunctionName)

	enc.AppendString(formattedCaller)
}

// zapWriter is an io.Writer that logs messages to Zap at a specified level.
// This allows redirecting standard library output or Gin's default output to Zap.
type ZapWriter struct {
	SugarLogger *zap.SugaredLogger
	Level       zapcore.Level
}

func (zw *ZapWriter) Write(p []byte) (n int, err error) {
	s := strings.TrimSpace(string(p))
	if s == "" { // Don't log empty lines
		return len(p), nil
	}
	switch zw.Level {
	case zap.DebugLevel:
		zw.SugarLogger.Debug(s)
	case zap.InfoLevel:
		zw.SugarLogger.Info(s)
	case zap.WarnLevel:
		zw.SugarLogger.Warn(s)
	case zap.ErrorLevel:
		zw.SugarLogger.Error(s)
	default:
		zw.SugarLogger.Info(s)
	}
	return len(p), nil
}
