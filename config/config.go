package config

import "github.com/kelseyhightower/envconfig"

type Config struct {
	Redis   *RedisConfig
	Logger  *SlogConfig
	Wowza   *WowzaConfig
	Address string `envconfig:"address" default:"localhost:1935"`
}

type RedisConfig struct {
	Type    string `json:"type" envconfig:"TYPE"`
	Address string `json:"address" envconfig:"ADDRESS"`
	Port    string `json:"port" envconfig:"PORT"`
}

type WowzaConfig struct {
	WowzaHost string `json:"wowza_host" envconfig:"WOWZA_HOST"`
}

type SlogConfig struct {
	Level       string `envconfig:"LEVEL" default:"info"`
	Path        string
	PrintStdOut bool `envconfig:"STDOUT" default:"true"`
	MaxSizeMb   int  `envconfig:"MAX_SIZE" default:"500"`
	MaxBackups  int  `envconfig:"MAX_BACKUPS" default:"20"`
}

var EnvConfig *Config

func InitConfig() error {
	var err error

	EnvConfig = &Config{}

	if err = envconfig.Process("", EnvConfig); err != nil {
		return err
	}

	redis := &RedisConfig{}
	if err = envconfig.Process("REDIS", redis); err != nil {
		return err
	}

	slogConfig := &SlogConfig{}
	if err = envconfig.Process("SLOG", slogConfig); err != nil {
		return err
	}

	wowzaConfig := &WowzaConfig{}
	if err = envconfig.Process("WOWZA", wowzaConfig); err != nil {
		return err
	}

	EnvConfig.Redis = redis
	EnvConfig.Logger = slogConfig
	EnvConfig.Wowza = wowzaConfig

	return nil
}
