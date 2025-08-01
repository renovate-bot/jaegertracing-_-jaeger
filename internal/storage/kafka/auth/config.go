// Copyright (c) 2019 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"
	"strings"

	"github.com/Shopify/sarama"
	"github.com/spf13/viper"
	"go.opentelemetry.io/collector/config/configtls"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/internal/config/tlscfg"
)

const (
	none      = "none"
	kerberos  = "kerberos"
	tls       = "tls"
	plaintext = "plaintext"
)

var authTypes = []string{
	none,
	kerberos,
	tls,
	plaintext,
}

// AuthenticationConfig describes the configuration properties needed authenticate with kafka cluster
type AuthenticationConfig struct {
	Authentication string
	Kerberos       KerberosConfig
	TLS            configtls.ClientConfig
	PlainText      PlainTextConfig
}

// SetConfiguration set configure authentication into sarama config structure
func (config *AuthenticationConfig) SetConfiguration(saramaConfig *sarama.Config, logger *zap.Logger) error {
	authentication := strings.ToLower(config.Authentication)
	if strings.Trim(authentication, " ") == "" {
		authentication = none
	}

	// Apply TLS configuration if authentication is tls or if TLS is explicitly enabled
	if config.Authentication == tls || !config.TLS.Insecure {
		if err := setTLSConfiguration(&config.TLS, saramaConfig, logger); err != nil {
			return err
		}
	}

	switch authentication {
	case none:
		return nil
	case tls:
		return nil
	case kerberos:
		setKerberosConfiguration(&config.Kerberos, saramaConfig)
		return nil
	case plaintext:
		return setPlainTextConfiguration(&config.PlainText, saramaConfig)
	default:
		return fmt.Errorf("Unknown/Unsupported authentication method %s to kafka cluster", config.Authentication)
	}
}

// InitFromViper loads authentication configuration from viper flags.
func (config *AuthenticationConfig) InitFromViper(configPrefix string, v *viper.Viper) error {
	config.Authentication = v.GetString(configPrefix + suffixAuthentication)
	config.Kerberos.ServiceName = v.GetString(configPrefix + kerberosPrefix + suffixKerberosServiceName)
	config.Kerberos.Realm = v.GetString(configPrefix + kerberosPrefix + suffixKerberosRealm)
	config.Kerberos.UseKeyTab = v.GetBool(configPrefix + kerberosPrefix + suffixKerberosUseKeyTab)
	config.Kerberos.Username = v.GetString(configPrefix + kerberosPrefix + suffixKerberosUsername)
	config.Kerberos.Password = v.GetString(configPrefix + kerberosPrefix + suffixKerberosPassword)
	config.Kerberos.ConfigPath = v.GetString(configPrefix + kerberosPrefix + suffixKerberosConfig)
	config.Kerberos.KeyTabPath = v.GetString(configPrefix + kerberosPrefix + suffixKerberosKeyTab)
	config.Kerberos.DisablePAFXFast = v.GetBool(configPrefix + kerberosPrefix + suffixKerberosDisablePAFXFAST)

	// Always try to load TLS configuration from viper
	tlsClientConfig := tlscfg.ClientFlagsConfig{
		Prefix: configPrefix,
	}
	tlsCfg, err := tlsClientConfig.InitFromViper(v)
	if err != nil {
		return fmt.Errorf("failed to process Kafka TLS options: %w", err)
	}

	// Configure TLS settings based on authentication type and TLS enablement
	if config.Authentication == tls {
		// for TLS authentication, ensure TLS is secure and include system CAs
		tlsCfg.Insecure = false
		tlsCfg.IncludeSystemCACertsPool = true
	} else if v.GetBool(configPrefix + ".tls.enabled") {
		// for non-TLS authentication with TLS enabled, ensure TLS is secure and include system CAs
		tlsCfg.Insecure = false
		tlsCfg.IncludeSystemCACertsPool = true
	}
	// for non-TLS authentication without TLS enabled, keep OTEL defaults (Insecure=false, IncludeSystemCACertsPool=false)
	config.TLS = tlsCfg

	config.PlainText.Username = v.GetString(configPrefix + plainTextPrefix + suffixPlainTextUsername)
	config.PlainText.Password = v.GetString(configPrefix + plainTextPrefix + suffixPlainTextPassword)
	config.PlainText.Mechanism = v.GetString(configPrefix + plainTextPrefix + suffixPlainTextMechanism)
	return nil
}
