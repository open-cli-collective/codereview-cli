package initcmd

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
	ctx.Config = config.Normalize(ctx.Config)
	ref := strings.TrimSpace(ctx.Entry.Ref.Ref)
	if ref == "" {
		ref = "(standard credential location)"
	}
	destination, details := initCredentialDestinationDetails(ctx, ref)
	lines := []string{"Destination: " + destination}
	lines = append(lines, details...)
	lines = append(lines, "Secret values are collected separately.")
	return strings.Join(lines, "\n")
}

func initCredentialDestinationDetails(ctx initCredentialDestinationContext, ref string) (string, []string) {
	storeID := strings.TrimSpace(ctx.Entry.Ref.Store)
	if storeID == "" {
		storeID = strings.TrimSpace(ctx.Entry.SecretsStore.ID)
	}
	if storeID == "" {
		return initLegacyCredentialDestinationDetails(ctx, ref)
	}
	location := config.CredentialLocation{Store: storeID, Name: ref}
	lines := []string{}
	if value := strings.TrimSpace(storeID); value != "" {
		lines = append(lines, "Credential store: "+value)
	}
	if value := strings.TrimSpace(ctx.Entry.SecretsStore.Backend); value != "" {
		lines = append(lines, "Backend kind: "+value)
	}
	destination := initCredentialDestinationLabel(ctx.Config, location)
	if storeID == config.LocalOSCredentialStoreID && ctx.BackendFlagSet {
		if backend, _, err := credentials.BackendMetadata(ctx.BackendArg, ctx.BackendFlagSet, ctx.Config); err == nil && strings.TrimSpace(string(backend)) != "" {
			for i, line := range lines {
				if strings.HasPrefix(line, "Backend kind: ") {
					lines[i] = "Backend kind: " + string(backend)
					break
				}
			}
		}
	}
	if profile, ok := ctx.Config.Secrets.Stores[storeID]; ok {
		lines = append(lines, initOnePasswordDestinationDetails(profile.Backend)...)
	} else if storeID != config.LocalOSCredentialStoreID {
		if displayName := strings.TrimSpace(ctx.Entry.SecretsStore.DisplayName()); displayName != "" {
			destination = strings.Join([]string{displayName, ref}, " / ")
		}
		lines = append(lines, "credential destination unavailable.")
	}
	return destination, lines
}

func initLegacyCredentialDestinationDetails(ctx initCredentialDestinationContext, ref string) (string, []string) {
	displayName := initBuiltInOSCredentialStoreTitle()
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

func initOnePasswordDestinationDetails(backend config.SecretsStoreBackend) []string {
	if !config.IsOnePasswordSecretsBackend(backend.Kind) || backend.OnePassword == nil {
		return nil
	}
	onePassword := backend.OnePassword
	lines := []string{}
	if value := strings.TrimSpace(onePassword.VaultName); value != "" {
		lines = append(lines, "1Password vault: "+value)
	}
	if value := strings.TrimSpace(onePassword.VaultID); value != "" {
		lines = append(lines, "1Password vault: "+value)
	}
	if value := strings.TrimSpace(onePassword.AccountURL); value != "" {
		lines = append(lines, "1Password account: "+value)
	}
	if value := strings.TrimSpace(onePassword.AccountID); value != "" {
		lines = append(lines, "1Password account id: "+value)
	}
	if value := strings.TrimSpace(onePassword.ConnectHost); value != "" {
		lines = append(lines, "1Password Connect host: "+value)
	}
	if value := strings.TrimSpace(onePassword.ServiceTokenEnv); value != "" {
		lines = append(lines, "1Password service account token env var: "+value)
	}
	if value := strings.TrimSpace(onePassword.ConnectTokenEnv); value != "" {
		lines = append(lines, "1Password Connect token env var: "+value)
	}
	if value := strings.TrimSpace(onePassword.DesktopAccountID); value != "" {
		lines = append(lines, "1Password desktop account id: "+value)
	}
	return lines
}
