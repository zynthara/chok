package app

import "github.com/zynthara/chok/config"

// Config is the blog application configuration.
type Config struct {
	HTTP    config.HTTPOptions    `mapstructure:"http"`
	Log     config.SlogOptions    `mapstructure:"log"`
	SQLite  config.SQLiteOptions  `mapstructure:"sqlite"`
	Account config.AccountOptions `mapstructure:"account"`
	Swagger config.SwaggerOptions `mapstructure:"swagger"`
}
