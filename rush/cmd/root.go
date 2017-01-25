// Copyright © 2017 Wei Shen <shenwei356@gmail.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "rush",
	Short: "parallelly execute shell commands",
	Long: fmt.Sprintf(`
rush -- parallelly execute shell commands

Version: %s

Author: Wei Shen <shenwei356@gmail.com>

Source code: https://github.com/shenwei356/rush

`, VERSION),
	Run: func(cmd *cobra.Command, args []string) {
		var err error
		config := getConfigs(cmd)

		if config.Version {
			checkVersion()
			return
		}

		j := config.Jobs + 1
		if j > runtime.NumCPU() {
			j = runtime.NumCPU()
		}
		runtime.GOMAXPROCS(j)

		config.reFieldDelimiter, err = regexp.Compile(config.FieldDelimiter)
		checkError(errors.Wrap(err, "compile field delimiter"))

		command0 := strings.Join(args, " ")
		if command0 == "" {
			command0 = `echo "{}"`
		}

		// -----------------------------------------------------------------

		// out file handler
		var outfh *bufio.Writer
		if isStdin(config.OutFile) {
			outfh = bufio.NewWriter(os.Stdout)
		} else {
			var fh *os.File
			fh, err = os.Create(config.OutFile)
			checkError(err)
			defer fh.Close()

			outfh = bufio.NewWriter(fh)
		}
		defer outfh.Flush()

		// TmpOutputDataBuffer = config.BufferSize
		if config.DryRun {
			config.Verbose = false
		}

		var succCmds = make(map[string]struct{})
		var bfhSuccCmds *bufio.Writer
		if config.Continue {
			var existed bool
			existed, err = exists(config.SuccCmdFile)
			checkError(err)
			if existed {
				succCmds = readSuccCmds(config.SuccCmdFile)
			}

			var fhSuccCmds *os.File
			fhSuccCmds, err = os.Create(config.SuccCmdFile)
			checkError(err)
			defer fhSuccCmds.Close()

			bfhSuccCmds = bufio.NewWriter(fhSuccCmds)
			defer bfhSuccCmds.Flush()
		}

		opts := &Options{
			DryRun:              config.DryRun,
			Jobs:                config.Jobs,
			KeepOrder:           config.KeepOrder,
			Retries:             config.Retries,
			RetryInterval:       time.Duration(config.RetryInterval) * time.Second,
			Timeout:             time.Duration(config.Timeout) * time.Second,
			StopOnErr:           config.StopOnErr,
			Verbose:             config.Verbose,
			RecordSuccessfulCmd: config.Continue,
		}

		if len(config.Infiles) == 0 {
			config.Infiles = append(config.Infiles, "-")
		}

		// split function for scanner
		recordDelimiter := []byte(config.RecordDelimiter)
		split := func(data []byte, atEOF bool) (advance int, token []byte, err error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}
			i := bytes.Index(data, recordDelimiter)
			if i >= 0 {
				return i + len(recordDelimiter), data[0:i], nil // trim config.RecordDelimiter
			}
			if atEOF {
				return len(data), data, nil
			}
			return 0, nil, nil
		}

		// ---------------------------------------------------------------

		cancel := make(chan struct{})

		donePreprocessFiles := make(chan int)

		// channel of command
		chCmdStr := make(chan string, config.Jobs)

		// read data and generate command
		go func() {
			n := config.NRecords
			var id uint64 = 1

		READFILES:
			for _, file := range config.Infiles {
				// input file handler
				var infh *os.File
				if isStdin(file) {
					infh = os.Stdin
				} else {
					infh, err = os.Open(file)
					checkError(err)
					defer infh.Close()
				}

				scanner := bufio.NewScanner(infh)
				// 2147483647: max int32
				scanner.Buffer(make([]byte, 0, 16384), 2147483647)
				scanner.Split(split)

				var record string
				var records []string
				records = make([]string, 0, n)
				var cmdStr string
				var runned bool
				for scanner.Scan() {
					select {
					case <-cancel:
						if config.Verbose {
							log.Warningf("cancel reading file: %s", file)
						}
						break READFILES
					default:
					}

					record = scanner.Text()
					if record == "" {
						continue
					}
					records = append(records, record)

					if len(records) == n {
						cmdStr = fillCommand(config, command0, Chunk{ID: id, Data: records})
						if config.Continue {
							if _, runned = succCmds[cmdStr]; runned {
								log.Infof("ignore cmd #%d: %s", id, cmdStr)
								bfhSuccCmds.WriteString(cmdStr + endMarkOfCMD)
							} else {
								chCmdStr <- cmdStr
							}
						} else {
							chCmdStr <- cmdStr
						}

						id++
						records = make([]string, 0, n)
					}
				}
				if len(records) > 0 {
					cmdStr = fillCommand(config, command0, Chunk{ID: id, Data: records})
					if config.Continue {
						if _, runned = succCmds[cmdStr]; runned {
							log.Infof("ignore cmd #%d: %s", id, cmdStr)
							bfhSuccCmds.WriteString(cmdStr + endMarkOfCMD)
						} else {
							chCmdStr <- cmdStr
						}
					} else {
						chCmdStr <- cmdStr
					}
				}
				if config.Continue {
					bfhSuccCmds.Flush()
				}
				checkError(errors.Wrap(scanner.Err(), "read input data"))
			}

			close(chCmdStr)
			// if Verbose {
			// 	log.Infof("finish reading input data")
			// }
			donePreprocessFiles <- 1
		}()

		// ---------------------------------------------------------------

		// run
		chOutput, chSuccessfulCmd, doneSendOutput := Run4Output(opts, cancel, chCmdStr)

		// read from chOutput and print
		doneOutput := make(chan int)
		go func() {
			last := time.Now().Add(2 * time.Second)
			for c := range chOutput {
				outfh.WriteString(c)
				if t := time.Now(); t.After(last) {
					outfh.Flush()
					last = t.Add(2 * time.Second)
				}
			}
			outfh.Flush()

			doneOutput <- 1
		}()

		doneSaveSuccCmd := make(chan int)
		if config.Continue {
			go func() {
				for c := range chSuccessfulCmd {
					bfhSuccCmds.WriteString(c + endMarkOfCMD)
					bfhSuccCmds.Flush()
				}
				doneSaveSuccCmd <- 1
			}()
		}

		// ---------------------------------------------------------------

		chExitSignalMonitor := make(chan struct{})
		signalChan := make(chan os.Signal, 1)
		cleanupDone := make(chan int)
		signal.Notify(signalChan, os.Interrupt)
		go func() {
			select {
			case <-signalChan:
				log.Criticalf("received an interrupt, stopping unfinished commands...")
				select {
				case <-cancel: // already closed
				default:
					close(cancel)
				}
				cleanupDone <- 1
				return
			case <-chExitSignalMonitor:
				cleanupDone <- 1
				return
			}
		}()

		// the order is very important!
		<-donePreprocessFiles // finish read data and send command
		<-doneSendOutput      // finish send output
		<-doneOutput          // finish print output
		if config.Continue {
			<-doneSaveSuccCmd
		}

		close(chExitSignalMonitor)
		<-cleanupDone
	},
}

// Chunk contains input data records sent to a command
type Chunk struct {
	ID   uint64
	Data []string
}

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(-1)
	}
}

func init() {
	RootCmd.Flags().BoolP("verbose", "", false, "print verbose information")
	RootCmd.Flags().BoolP("version", "V", false, `print version information and check for update`)

	RootCmd.Flags().IntP("jobs", "j", runtime.NumCPU(), "run n jobs in parallel (default value depends on your device)")
	RootCmd.Flags().StringP("out-file", "o", "-", `out file ("-" for stdout)`)

	RootCmd.Flags().StringSliceP("infile", "i", []string{}, "input data file, multi-values supported")

	RootCmd.Flags().StringP("record-delimiter", "D", "\n", `record delimiter (default is "\n")`)
	RootCmd.Flags().StringP("records-join-sep", "J", "\n", `record separator for joining multi-records (default is "\n")`)
	RootCmd.Flags().IntP("nrecords", "n", 1, "number of records sent to a command")
	RootCmd.Flags().StringP("field-delimiter", "d", `\s+`, "field delimiter in records, support regular expression")

	RootCmd.Flags().IntP("retries", "r", 0, "maximum retries (default 0)")
	RootCmd.Flags().IntP("retry-interval", "", 0, "retry interval (unit: second) (default 0)")
	RootCmd.Flags().IntP("timeout", "t", 0, "timeout of a command (unit: second, 0 for no timeout) (default 0)")

	RootCmd.Flags().BoolP("keep-order", "k", false, "keep output in order of input")
	RootCmd.Flags().BoolP("stop-on-error", "e", false, "stop all processes on first error(s)")
	RootCmd.Flags().BoolP("dry-run", "", false, "print command but not run")

	RootCmd.Flags().BoolP("continue", "c", false, `continue jobs.`+
		` NOTES: 1) successful commands is saved in file (given by flag -C/--succ-cmd-file);`+
		` 2) if the file does not exists, rush saves data so we can continue jobs next time;`+
		` 3) if the file exists, rush ignores jobs in it`)
	RootCmd.Flags().StringP("succ-cmd-file", "C", "successful_cmds.rush", `file for saving successful commands`)

	// RootCmd.Flags().IntP("buffer-size", "", 1, "buffer size for output of a command before saving to tmpfile (unit: Mb)")

	RootCmd.Flags().StringSliceP("assign", "v", []string{}, "assign the value val to the variable var (format: var=val)")
	RootCmd.Flags().StringP("trim", "", "", `trim white space in input (available values: "l" for left, "r" for right, "lr", "rl", "b" for both side)`)

	RootCmd.Example = `  1. simple run, quoting is not necessary
      $ seq 1 10 | rush echo {}
  2. keep order
      $ seq 1 10 | rush 'echo {}' -k
  3. timeout
      $ seq 1 | rush 'sleep 2; echo {}' -t 1
  4. retry
      $ seq 1 | rush 'python script.py' -r 3
  5. dirname & basename & remove suffix
      $ echo dir/file_1.txt.gz | rush 'echo {/} {%} {^_1.txt.gz}'
      dir file.txt.gz dir/file
  6. basename without last or any extension
      $ echo dir.d/file.txt.gz | rush 'echo {.} {:} {%.} {%:}'
      dir.d/file.txt dir.d/file file.txt file
  7. job ID, combine fields and other replacement strings
      $ echo 12 file.txt dir/s_1.fq.gz | rush 'echo job {#}: {2} {2.} {3%:^_1}'
      job 1: file.txt file s
  8. custom field delimiter
      $ echo a=b=c | rush 'echo {1} {2} {3}' -d =
      a b c
  9. assign value to variable, like "awk -v"
      $ seq 1 | rush 'echo Hello, {fname} {lname}!' -v fname=Wei -v lname=Shen
      Hello, Wei Shen!
  10. save successful commands to continue in NEXT run
      $ seq 1 3 | rush 'sleep {}; echo {}' -c -t 2
      [INFO] ignore cmd #1: sleep 1; echo 1
      [ERRO] run cmd #1: sleep 2; echo 2: time out
      [ERRO] run cmd #2: sleep 3; echo 3: time out

  More examples: https://github.com/shenwei356/rush`

	RootCmd.SetUsageTemplate(`Usage:{{if .Runnable}}
  {{if .HasAvailableFlags}}{{appendIfNotPresent .UseLine "[flags]"}}{{else}}{{.UseLine}}{{end}}{{end}}{{if .HasAvailableSubCommands}}
  {{ .CommandPath}} [command]{{end}} [command] [args of command...]{{if gt .Aliases 0}}

Aliases:
  {{.NameAndAliases}}
{{end}}{{if .HasExample}}

Examples:
{{ .Example }}{{end}}{{ if .HasAvailableSubCommands}}

Available Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{ if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimRightSpace}}{{end}}{{ if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimRightSpace}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsHelpCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{ if .HasAvailableSubCommands }}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`)
}

// Config is the struct containing all global flags
type Config struct {
	Verbose bool
	Version bool

	Jobs    int
	OutFile string

	Infiles []string

	RecordDelimiter      string
	RecordsJoinSeparator string
	NRecords             int
	FieldDelimiter       string
	reFieldDelimiter     *regexp.Regexp

	Retries       int
	RetryInterval int
	Timeout       int

	KeepOrder bool
	StopOnErr bool
	DryRun    bool

	Continue    bool
	SuccCmdFile string

	AssignMap map[string]string
	Trim      string
}

// var=value
var reAssign = regexp.MustCompile(`\s*([^=]+)\s*=(.+)`)

func getConfigs(cmd *cobra.Command) Config {
	trim := getFlagString(cmd, "trim")
	if trim != "" {
		trim = strings.ToLower(trim)
		switch trim {
		case "l":
		case "r":
		case "lr", "rl", "b":
		default:
			checkError(fmt.Errorf(`illegal value for flag --trim: %s. (available values: "l" for left, "r" for right, "lr", "rl", "b" for both side)`, trim))
		}
	}

	assignStrs := getFlagStringSlice(cmd, "assign")
	assignMap := make(map[string]string)
	for _, s := range assignStrs {
		if reAssign.MatchString(s) {
			found := reAssign.FindStringSubmatch(s)
			switch found[1] {
			case ".", ":", "/", "%", "#", "^":
				checkError(fmt.Errorf(`"var" in --v/--assign var=val should not be ".", ":", "/", "%%", "^" or "#", given: "%s"`, found[1]))
			}
			assignMap[found[1]] = found[2]
		} else {
			checkError(fmt.Errorf(`illegal value for flag -v/--assign (format: "var=value", e.g., "-v 'a=a bc'"): %s`, s))
		}
	}

	return Config{
		Verbose: getFlagBool(cmd, "verbose"),
		Version: getFlagBool(cmd, "version"),

		Jobs:    getFlagPositiveInt(cmd, "jobs"),
		OutFile: getFlagString(cmd, "out-file"),

		Infiles: getFlagStringSlice(cmd, "infile"),

		RecordDelimiter:      getFlagString(cmd, "record-delimiter"),
		RecordsJoinSeparator: getFlagString(cmd, "records-join-sep"),
		NRecords:             getFlagPositiveInt(cmd, "nrecords"),
		FieldDelimiter:       getFlagString(cmd, "field-delimiter"),

		Retries:       getFlagNonNegativeInt(cmd, "retries"),
		RetryInterval: getFlagNonNegativeInt(cmd, "retry-interval"),
		Timeout:       getFlagNonNegativeInt(cmd, "timeout"),

		KeepOrder: getFlagBool(cmd, "keep-order"),
		StopOnErr: getFlagBool(cmd, "stop-on-error"),
		DryRun:    getFlagBool(cmd, "dry-run"),

		Continue:    getFlagBool(cmd, "continue"),
		SuccCmdFile: getFlagString(cmd, "succ-cmd-file"),

		Trim:      trim,
		AssignMap: assignMap,
	}
}
