package app

import "github.com/zynthara/chok/config"

// Config is the blog application configuration.
// Uses DatabaseOptions with a driver discriminator instead of bare
// SQLiteOptions — the recommended pattern for new projects.
type Config struct {
	HTTP     config.HTTPOptions     `mapstructure:"http"`
	Log      config.SlogOptions     `mapstructure:"log"`
	Database config.DatabaseOptions `mapstructure:"database"`
	Account  config.AccountOptions  `mapstructure:"account"`
	Swagger  config.SwaggerOptions  `mapstructure:"swagger"`
}
