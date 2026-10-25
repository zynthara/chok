package mail

import (
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"
)

// Config is the mail section of an application's configuration:
//
//	mail:
//	  from: Shop <no-reply@shop.example>
//	  smtp:
//	    host: smtp.example.com
//	    username: apikey
//
// The SMTP password is a secret: set it from the environment
// (SHOP_MAIL_SMTP_PASSWORD for an app loaded as "shop"), never in a file.
// New gives an empty Transport or TLS and a zero Timeout or Port the
// defaults below, whether the Config comes from a file or from code.
type Config struct {
	// Transport is how a mail leaves: smtp sends it; dir writes it as an
	// .eml file into Dir, for development, examples and tests; log only
	// logs it. A Sender option replaces the transport, and then
	// Transport and its settings are ignored.
	Transport string `yaml:"transport" default:"smtp" validate:"omitempty,oneof=smtp dir log"`
	// From is the sender, "Name <addr>" or "addr"; required. Message-IDs
	// take its domain.
	From string `yaml:"from"`
	// ReplyTo is where replies go, when not to From.
	ReplyTo string `yaml:"reply_to"`
	// Dir is where transport dir writes, one {unix nanoseconds}-{random}.eml
	// file per mail; required with it, and created by New.
	Dir string `yaml:"dir"`
	// Timeout bounds one Send: dialing, TLS, authentication and delivery.
	Timeout time.Duration `yaml:"timeout" default:"15s"`
	// SMTP is the relay transport smtp sends through.
	SMTP SMTPConfig `yaml:"smtp"`
}

// SMTPConfig is mail.smtp: the relay a transport smtp mail goes through,
// such as Resend's, Postmark's or Amazon SES's. Each Send dials it anew.
type SMTPConfig struct {
	// Host is the relay; required with transport smtp.
	Host string `yaml:"host"`
	// Port is the relay's port: 587 with starttls, 465 with implicit.
	Port int `yaml:"port" default:"587"`
	// Username and Password authenticate with AUTH PLAIN, over TLS only
	// (or to a loopback host); an empty Username skips authentication.
	Username string `yaml:"username"`
	Password string `yaml:"password" secret:"true"`
	// TLS is starttls (upgrade the plain connection; the relay must offer
	// it), implicit (TLS from the first byte) or none, which only a
	// loopback host may use, for local debugging.
	TLS string `yaml:"tls" default:"starttls" validate:"omitempty,oneof=starttls implicit none"`
}

// The defaults New applies to the zero fields of a Config built in code.
const (
	defaultTransport = "smtp"
	defaultTimeout   = 15 * time.Second
	defaultPort      = 587
	defaultTLS       = "starttls"
)

// withDefaults fills what a Config built in code leaves at zero; one from
// chok.Load already carries every default.
func (c Config) withDefaults() Config {
	if c.Transport == "" {
		c.Transport = defaultTransport
	}
	if c.Timeout == 0 {
		c.Timeout = defaultTimeout
	}
	if c.SMTP.Port == 0 {
		c.SMTP.Port = defaultPort
	}
	if c.SMTP.TLS == "" {
		c.SMTP.TLS = defaultTLS
	}
	return c
}

// Validate checks the addresses and the numbers. It runs when chok.Load
// reads the configuration and again in New; what a transport needs —
// smtp's host, dir's directory — New checks, unless a Sender replaces
// the transport.
func (c *Config) Validate() error {
	var errs []string
	if strings.TrimSpace(c.From) == "" {
		errs = append(errs, "from is required: the sender's address, such as Shop <no-reply@shop.example>")
	} else if _, err := mail.ParseAddress(c.From); err != nil {
		errs = append(errs, fmt.Sprintf("from %q is not an address: %v", c.From, err))
	}
	if c.ReplyTo != "" {
		if _, err := mail.ParseAddress(c.ReplyTo); err != nil {
			errs = append(errs, fmt.Sprintf("reply_to %q is not an address: %v", c.ReplyTo, err))
		}
	}
	if c.Timeout < 0 {
		errs = append(errs, "timeout must not be negative")
	}
	if c.SMTP.Port < 0 || c.SMTP.Port > 65535 {
		errs = append(errs, fmt.Sprintf("smtp.port %d is not a port", c.SMTP.Port))
	}
	if len(errs) == 0 {
		return nil
	}
	slices.Sort(errs)
	return errors.New(strings.Join(errs, "; "))
}
