package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

// completionCmd replaces cobra's auto-generated command so the help can
// carry per-shell install instructions; the generators are still cobra's.
var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate shell completion scripts",
	Long: `Generate a shell completion script for kiac.

Zsh:
  kiac completion zsh > $(brew --prefix)/share/zsh/site-functions/_kiac
  # start a new shell; if completions stay off, add to ~/.zshrc first:
  #   autoload -U compinit; compinit

Bash:
  # needs the bash-completion package (brew install bash-completion@2)
  kiac completion bash > $(brew --prefix)/etc/bash_completion.d/kiac

Fish:
  kiac completion fish > ~/.config/fish/completions/kiac.fish

PowerShell:
  kiac completion powershell | Out-String | Invoke-Expression

To try it in the current shell without installing anything:
  source <(kiac completion bash)   # bash
  source <(kiac completion zsh)    # zsh`,
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
	Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return rootCmd.GenBashCompletionV2(os.Stdout, true)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		default:
			return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
		}
	},
}

func init() { rootCmd.AddCommand(completionCmd) }
