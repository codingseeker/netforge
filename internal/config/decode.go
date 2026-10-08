package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codingseeker/netforge/internal/apperr"
)

const (
	StdinPath = "-"

	MaxPathLength = 4096
)

func Parse(data []byte) (*Config, error) {
	if len(data) < MinYAMLBytes {
		return nil, apperr.InvalidConfig("config.Parse", "configuration is empty", nil)
	}
	if len(data) > MaxConfigBytes {
		return nil, apperr.LimitExceeded("config.Parse",
			"configuration size "+strconv.Itoa(len(data))+" bytes exceeds the maximum of "+strconv.Itoa(MaxConfigBytes)+" bytes", nil)
	}
	return decode(bytes.NewReader(data), "config.Parse")
}

func ParseReader(r io.Reader, op string) (*Config, error) {
	if r == nil {
		return nil, apperr.InvalidArgument(op, "reader must not be nil", nil)
	}
	limited := io.LimitReader(r, MaxConfigBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, readError(op, "cannot read configuration", err)
	}
	if len(data) > MaxConfigBytes {
		return nil, apperr.LimitExceeded(op,
			"configuration exceeds the maximum size of "+strconv.Itoa(MaxConfigBytes)+" bytes", nil)
	}
	return Parse(data)
}

func Load(path string) (*Config, error) {
	return LoadFrom(path, os.Stdin)
}

func LoadFrom(path string, stdin io.Reader) (*Config, error) {
	if path == "" {
		return nil, apperr.InvalidArgument("config.Load", "path must not be empty", nil)
	}
	if strings.ContainsRune(path, 0) {
		return nil, apperr.InvalidArgument("config.Load", "path must not contain NUL bytes", nil)
	}
	if len(path) > MaxPathLength {
		return nil, apperr.InvalidArgument("config.Load", "path exceeds the maximum length of "+strconv.Itoa(MaxPathLength)+" bytes", nil)
	}

	if path == StdinPath {
		if stdin == nil {
			return nil, apperr.InvalidArgument("config.Load", "standard input is not available", nil)
		}
		cfg, err := ParseReader(stdin, "config.Load")
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, openError(path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, readError("config.Load", "cannot inspect configuration file "+strconv.Quote(path), err)
	}
	if info.IsDir() {
		return nil, apperr.InvalidArgument("config.Load", "configuration path "+strconv.Quote(path)+" is a directory", nil)
	}
	if !info.Mode().IsRegular() {
		return nil, apperr.InvalidArgument("config.Load", "configuration path "+strconv.Quote(path)+" is not a regular file", nil)
	}
	if info.Size() > MaxConfigBytes {
		return nil, apperr.LimitExceeded("config.Load",
			"configuration file "+strconv.Quote(path)+" is "+strconv.FormatInt(info.Size(), 10)+" bytes, which exceeds the maximum of "+strconv.Itoa(MaxConfigBytes)+" bytes", nil)
	}

	cfg, err := ParseReader(file, "config.Load")
	if err != nil {
		return nil, decoratePath(err, path)
	}
	return cfg, nil
}

func Marshal(cfg Config) ([]byte, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, apperr.Internal("config.Marshal", "cannot serialize configuration", err)
	}
	return data, nil
}

func decode(r io.Reader, op string) (*Config, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, apperr.InvalidConfig(op, "configuration document is empty", err)
		}
		return nil, apperr.InvalidConfig(op, "cannot decode configuration: "+cleanYAMLError(err), err)
	}

	var extra yaml.Node
	switch err := decoder.Decode(&extra); {
	case err == nil:
		return nil, apperr.InvalidConfig(op, "configuration must contain exactly one YAML document", nil)
	case errors.Is(err, io.EOF):
	default:
		return nil, apperr.InvalidConfig(op, "cannot decode configuration: "+cleanYAMLError(err), err)
	}

	cfg.Limits = cfg.Limits.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func openError(path string, err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return apperr.NotFound("config.Load", "configuration file "+strconv.Quote(path)+" does not exist", err)
	case errors.Is(err, os.ErrPermission):
		return apperr.Permission("config.Load", "permission denied reading "+strconv.Quote(path), err)
	default:
		return apperr.IO("config.Load", "cannot open configuration file "+strconv.Quote(path), err)
	}
}

func readError(op, msg string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return apperr.Permission(op, msg, err)
	}
	return apperr.IO(op, msg, err)
}

func cleanYAMLError(err error) string {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		messages := make([]string, 0, len(typeErr.Errors))
		for _, message := range typeErr.Errors {
			messages = append(messages, message)
		}
		return strings.Join(messages, "; ")
	}
	return err.Error()
}

func decoratePath(err error, path string) error {
	if apperr.IsKind(err, apperr.KindLimitExceeded) {
		return err
	}
	return apperr.InvalidConfig("config.Load", "cannot load configuration file "+strconv.Quote(filepath.Base(path)), err)
}
