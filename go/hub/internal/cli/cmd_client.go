package cli

// The client commands (cli/client.rs): status, login, apps, deploy, logs, rollback.

import "github.com/spf13/cobra"

func statusCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "status [APP]", Short: "An app's state and Doctor (`kuben status shop/prod/web`); without an app, the service, admin account, cluster and console address of this server", RunE: notPorted}
}

func loginCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "login URL", Short: "Sign in to a Kuben server with an API token; later commands use it", RunE: notPorted}
}

func appsCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "apps", Short: "The apps you can see, with their state", RunE: notPorted}
}

func deployCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "deploy APP", Short: "Deploy an image to an app and wait until it runs", RunE: notPorted}
}

func logsCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "logs APP", Short: "An app's log lines; `-f` keeps following them", RunE: notPorted}
}

func rollbackCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "rollback APP", Short: "Return an app to an earlier revision", RunE: notPorted}
}
