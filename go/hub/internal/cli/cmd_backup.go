package cli

// `kuben backup` and `kuben restore` (cli/backup.rs).

import "github.com/spf13/cobra"

func backupCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "backup", Short: "Back up the database (and, if asked, the secret keyring) into a new directory under `backup.dir`", RunE: notPorted}
}

func restoreCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "restore", Short: "Restore a backup made by `kuben backup` into an empty database", RunE: notPorted}
}
