package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/roma2246/holdfast/internal/config"
)

func (a *app) initCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Создать holdfast.toml с настройками по умолчанию",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := a.repoRoot()
			if err != nil {
				return err
			}
			path, err := config.WriteTemplate(root, force)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "Создан %s\n", path)
			fmt.Fprintln(a.out, "Дальше: holdfast lint, holdfast guard install")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "перезаписать существующий файл")
	return cmd
}
