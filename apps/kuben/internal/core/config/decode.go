package config

import (
	"fmt"
	"math"
	"reflect"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/v2"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// Extract is the typed configuration in k. Keys nobody reads are ignored,
// key names match exactly, and nothing is converted silently: a string is
// not a number, a number not a boolean, one value not a list. The one
// leniency is for environment variables, where a bare number or boolean
// given to a text setting is taken as the text it was written as.
func Extract(k *koanf.Koanf) (Config, error) {
	var cfg Config
	if err := decode(k.Raw(), &cfg); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func decode(input, out any) error {
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			optionalHook,
			cookieSecureHook,
			envLiteralHook,
			integerHook,
			mapstructure.TextUnmarshallerHookFunc(),
		),
		Result:    out,
		TagName:   "koanf",
		MatchName: func(mapKey, fieldName string) bool { return mapKey == fieldName },
	})
	if err != nil {
		return err
	}
	return decoder.Decode(input)
}

// some decodes data as a T and wraps it; configuration has no null, so a
// key that is there is a value.
func some[T any](data any) (any, error) {
	var v T
	if err := decode(data, &v); err != nil {
		return nil, err
	}
	return opt.Some(v), nil
}

// optionalHook decodes into the opt.Val fields of the configuration.
func optionalHook(from, to reflect.Type, data any) (any, error) {
	if from == to {
		return data, nil
	}
	switch to {
	case reflect.TypeFor[opt.Val[string]]():
		return some[string](data)
	case reflect.TypeFor[opt.Val[uint64]]():
		return some[uint64](data)
	}
	return data, nil
}

// cookieSecureHook decodes `security.cookie_secure`.
func cookieSecureHook(_, to reflect.Type, data any) (any, error) {
	if to != reflect.TypeFor[CookieSecure]() {
		return data, nil
	}
	if literal, ok := data.(envLiteral); ok {
		data = literal.Value
	}
	return DecodeCookieSecure(data)
}

// envLiteralHook gives a text setting the text of a bare number or boolean
// from the environment, and every other setting its value.
func envLiteralHook(_, to reflect.Type, data any) (any, error) {
	literal, ok := data.(envLiteral)
	if !ok {
		return data, nil
	}
	if to.Kind() == reflect.String {
		return literal.Text, nil
	}
	return literal.Value, nil
}

// integerHook refuses what mapstructure would squeeze into an integer:
// decimals, and numbers beyond the range of the setting.
func integerHook(_, to reflect.Type, data any) (any, error) {
	var limit uint64
	switch to.Kind() {
	case reflect.Uint32:
		limit = math.MaxUint32
	case reflect.Uint64:
		limit = math.MaxUint64
	default:
		return data, nil
	}
	v := reflect.ValueOf(data)
	switch {
	case !v.IsValid():
		return data, nil
	case v.CanFloat():
		return nil, fmt.Errorf("expected a whole number, got %v", data)
	case v.CanInt() && (v.Int() < 0 || uint64(v.Int()) > limit): //nolint:gosec // negative ruled out first
		return nil, fmt.Errorf("%d is out of range (0 to %d)", v.Int(), limit)
	case v.CanUint() && v.Uint() > limit:
		return nil, fmt.Errorf("%d is out of range (0 to %d)", v.Uint(), limit)
	}
	return data, nil
}
