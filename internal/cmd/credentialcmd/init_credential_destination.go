package credentialcmd

import (
	"fmt"
	"strings"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

type initCredentialDestinationContext struct {
	Entry          initCredentialPlanEntry
	Config         config.File
	BackendArg     string
	BackendFlagSet bool
}

func initCredentialDestinationDescription(ctx initCredentialDestinationContext) string {
	ref := strings.TrimSpace(ctx.Entry.Ref.Ref)
	if ref == "" {
		ref = "(standard credential location)"
	}
	destination, details := initCredentialDestinationDetails(ctx, ref)
	lines := []string{"Destination: " + destination}
	lines = append(lines, details...)
	lines = append(lines, "Change destination by configuring/selecting a secrets-management profile; secret values are collected separately.")
	return strings.Join(lines, "\n")
}

func initCredentialDestinationDetails(ctx initCredentialDestinationContext, ref string) (string, []string) {
	resolved := ctx.Entry.SecretsProfile
	if resolved.IsNamed() {
		return initNamedCredentialDestinationDetails(ctx, ref)
	}
	return initLegacyCredentialDestinationDetails(ctx, ref)
}

func initNamedCredentialDestinationDetails(ctx initCredentialDestinationContext, ref string) (string, []string) {
	resolved := ctx.Entry.SecretsProfile
	displayName := strings.TrimSpace(resolved.DisplayName())
	if displayName == "" {
		displayName = "selected secrets-management profile"
	}
	profile, ok := ctx.Config.Secrets.Profiles[strings.TrimSpace(resolved.ID)]
	if !ok {
		return fmt.Sprintf("%s via %s", ref, displayName), []string{"credential destination unavailable."}
	}
	backendKind := profile.Backend.Kind
	if strings.TrimSpace(string(backendKind)) == "" {
		backendKind = config.SecretsBackendKind(resolved.Backend)
	}
	backendLabel := initSecretsBackendDisplayLabel(backendKind)
	lines := []string{}
	if strings.TrimSpace(resolved.ID) != "" {
		lines = append(lines, "Secrets profile: "+strings.TrimSpace(resolved.ID))
	}
	if strings.TrimSpace(string(backendKind)) != "" {
		lines = append(lines, "Backend kind: "+strings.TrimSpace(string(backendKind)))
	}
	lines = append(lines, initOnePasswordDestinationDetails(profile.Backend)...)
	return fmt.Sprintf("%s via %s (%s)", ref, displayName, backendLabel), lines
}

func initLegacyCredentialDestinationDetails(ctx initCredentialDestinationContext, ref string) (string, []string) {
	displayName := strings.TrimSpace(ctx.Entry.SecretsProfile.DisplayName())
	if displayName == "" {
		displayName = "Legacy default"
	}
	backend, source, err := credentials.BackendMetadata(ctx.BackendArg, ctx.BackendFlagSet, ctx.Config)
	if err != nil {
		return fmt.Sprintf("%s via %s", ref, displayName), []string{"credential destination unavailable."}
	}
	return fmt.Sprintf("%s via %s (%s)", ref, displayName, initCredentialBackendMetadataLabel(backend, source)), nil
}

func initCredentialBackendMetadataLabel(backend credstore.Backend, source credstore.Source) string {
	if source == credstore.SourceAuto {
		return initAutomaticOSDefaultSecretsBackendLabel()
	}
	if strings.TrimSpace(string(backend)) == "" {
		return initAutomaticOSDefaultSecretsBackendLabel()
	}
	return initSecretsBackendDisplayLabel(config.SecretsBackendKind(backend))
}

func initOnePasswordDestinationDetails(backend config.SecretsProfileBackend) []string {
	if !config.IsOnePasswordSecretsBackend(backend.Kind) || backend.OnePassword == nil {
		return nil
	}
	onePassword := backend.OnePassword
	lines := []string{}
	if value := strings.TrimSpace(onePassword.VaultID); value != "" {
		lines = append(lines, "1Password vault: "+value)
	}
	if value := strings.TrimSpace(onePassword.ItemTitlePrefix); value != "" {
		lines = append(lines, "1Password item title prefix: "+value)
	}
	if value := strings.TrimSpace(onePassword.ItemTag); value != "" {
		lines = append(lines, "1Password item tag: "+value)
	}
	if value := strings.TrimSpace(onePassword.ItemFieldTitle); value != "" {
		lines = append(lines, "1Password item field title: "+value)
	}
	if value := strings.TrimSpace(onePassword.ConnectHost); value != "" {
		lines = append(lines, "1Password Connect host: "+value)
	}
	if value := strings.TrimSpace(onePassword.ServiceTokenEnv); value != "" {
		lines = append(lines, "1Password backend auth env var: "+value)
	}
	if value := strings.TrimSpace(onePassword.ConnectTokenEnv); value != "" {
		lines = append(lines, "1Password backend auth env var: "+value)
	}
	if value := strings.TrimSpace(onePassword.DesktopAccountID); value != "" {
		lines = append(lines, "1Password desktop account id: "+value)
	}
	return lines
}
