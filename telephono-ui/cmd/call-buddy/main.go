package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	t "github.com/call-buddy/call-buddy/telephono"
	"github.com/call-buddy/gocui"
)

var profiles *t.CallBuddyProfiles
var stateDir string

func init() {
	if f, err := os.OpenFile("tui.log", os.O_RDWR|os.O_APPEND|os.O_CREATE, 0755); err != nil {
		panic(err.Error())
	} else {
		log.SetOutput(f)
	}

	log.Print("Starting up TCB")
	profiles = &t.CallBuddyProfiles{}
	stateDir = lookupStateDir()
	ok, errs := profiles.Init(stateDir)
	var tempErrs string
	for _, err := range errs {
		tempErrs += err.Error() + "\n"
	}
	if !ok {
		log.Fatalf("Critical Initialization Error(s): \n %s", tempErrs)
	} else {
		log.Printf("Non-Critical Initialization Error(s): \n %s", tempErrs)
	}
}

func lookupStateDir() (dir string) {
	var err error
	if dir = os.Getenv("XDG_DATA_HOME"); dir != "" {
		return
	}
	if dir, err = os.UserHomeDir(); err != nil {
		dir = "./" // Uhh no home dir?! TCB doesn't run on a potato
	}
	// Nothin' wrong with sticking this in home dirs right?! \s
	return filepath.Join(dir, ".call-buddy")
}

func getCurrentRequestTemplate(state *t.CallBuddyState) *t.RequestTemplate {
	// DEMO AH: Obviously this is NOT ok
	return profiles.CurrentState().Collections[0].RequestTemplates[0]
}

type HistView gocui.View

var theEditor TCBEditor

type TCBEditor struct {
	// This is super dumb that I have to embed this, no way to pass it in...
	gui               *gocui.Gui
	expectKeyModifier bool
}

func (editor *TCBEditor) Edit(v *gocui.View, key gocui.Key, ch rune, mod gocui.Modifier) {
	if editor.expectKeyModifier && ch == 'Z' { // Tab for some some some some dumb dumb dumb reason
		switchPrevView(editor.gui, v)
		editor.expectKeyModifier = false
		return
	}

	if ch == '[' && mod == gocui.ModAlt {
		editor.expectKeyModifier = true
		return
	}
	gocui.DefaultEditor.Edit(v, key, ch, mod)
}

// ViewState Which view is active
type ViewState int

const (
	// CMD_LINE The command line view is active
	CMD_LINE ViewState = iota
	// MTD_BODY The method body view is active (GET, PUT, HEAD)
	MTD_BODY
	// RQT_HEAD The request header view is active
	RQT_HEAD
	// RQT_BODY The request body view is active
	RQT_BODY
	// HIST_BODY The history body is active
	HIST_BODY
	// RSP_BODY The response body view is active
	RSP_BODY
	// NO_STATE No state is selected
	NO_STATE
)
const (
	TTL_LINE_VIEW = "title_view"
	// CMD_LINE_VIEW The command line view string
	CMD_LINE_VIEW = "command"
	// MTD_BODY_VIEW The method body view string
	MTD_BODY_VIEW = "method_body"
	// RQT_HEAD_VIEW The request header view string
	RQT_HEAD_VIEW = "request_head"
	// RQT_BODY_VIEW The request body view string
	RQT_BODY_VIEW = "request_body"
	// RSP_BODY_VIEW The response body view string
	RSP_BODY_VIEW = "response_body"
	// HIST_VIEW The history body view string
	HIST_VIEW = "history_body"
)

type ioHijacker struct {
	backupFile *os.File
	pipe       *os.File
	channel    chan string
}

var currView ViewState = NO_STATE //needs a better name

func die(msg string) {
	os.Stderr.WriteString(msg)
	os.Exit(1)
}

// saveResponseToFile Save response body to a file
func saveResponseToFile(contents, filepath string, appendToFile bool) (err error) {
	var fd *os.File
	if appendToFile {
		fd, err = os.OpenFile(filepath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	} else {
		fd, err = os.Create(filepath)
	}
	if err != nil {
		return
	}
	defer fd.Close()
	fd.WriteString(contents)
	return
}

func loadResponseFromFile(requestBodyView *gocui.View, filepath string) (err error) {
	response, err := ioutil.ReadFile(filepath)
	if err != nil {
		return
	}
	updateRequestBodyView(requestBodyView, string(response))
	return
}

func addUserEnvironmentVariable(kv string) {
	var splatted []string
	if splatted = strings.SplitN(kv, "=", 2); len(splatted) != 2 {
		return
	}
	profiles.CurrentState().Environment.User.Set(splatted[0], splatted[1])
}

// appendHeaderToView Adds a key=value header to the header view
func appendHeaderToView(kv string, requestHeaderView *gocui.View) {
	var splatted []string
	if splatted = strings.SplitN(kv, "=", 2); len(splatted) != 2 {
		return
	}

	// We want to set a header
	headers := getCurrentRequestTemplate(profiles.CurrentState()).Headers
	tmpHeaders := map[string][]string{}
	for key, values := range headers {
		tmpHeaders[key] = values
	}
	tmpHeaders[splatted[0]] = []string{splatted[1]}
	updateRequestHeaderView(requestHeaderView, tmpHeaders)
}

func getHeadersFromView(headerBody string) (headers http.Header, errs []error) {
	rawHeaders := strings.Split(headerBody, "\n")
	if len(rawHeaders) == 0 {
		return
	}

	headers = http.Header{}
	for _, rawHeader := range rawHeaders {
		var splatted []string
		if strings.TrimSpace(rawHeader) == "" {
			continue
		}

		if splatted = strings.SplitN(rawHeader, ": ", 2); len(splatted) != 2 {
			errs = append(errs, errors.New("Could not split "+rawHeader))
			continue
		}
		headers.Add(splatted[0], splatted[1])
	}
	return
}

// TODO AH: args should probably get broken out into real parameters
func call(methodType, url, body, headerBody string) (t.HistoricalCall, error) {
	methodType = strings.ToLower(methodType)
	// TODO AH: Clean up documentation and other places
	//contentType := "text/plain"

	theTemplate := getCurrentRequestTemplate(profiles.CurrentState())
	theTemplate.Method = t.HttpMethod(strings.ToUpper(methodType))
	theTemplate.Body = body
	theTemplate.Url = url
	headers, errs := getHeadersFromView(headerBody)
	if len(errs) != 0 {
		var combinedErr string
		for i, err := range errs {
			combinedErr += err.Error()
			if i != len(errs)-1 {
				combinedErr += "\n"
			}
		}
		return t.HistoricalCall{}, errors.New(combinedErr)
	}
	theTemplate.Headers = headers
	return theTemplate.Execute(http.DefaultClient, &profiles.CurrentState().Environment)
}

func enterHistoryView(g *gocui.Gui) {
	//Locking here to stop race conditions that can prevent the view
	//from being present before we set keybindings

	g.SetManagerFunc(histLayout)
	/*g.Update(func(gui *gocui.Gui) error {
		gui.SetManagerFunc(histLayout)
		return nil
	})*/

	g.Update(func(gui *gocui.Gui) error {
		setView(gui, HIST_VIEW, HIST_BODY)
		return nil
	})
	g.Update(func(gui *gocui.Gui) error {
		histView, _ := gui.View(HIST_VIEW)
		updateHistoryView(histView)
		return nil
	})

	g.Update(func(gui *gocui.Gui) error {
		if profiles.CurrentState().History.Size() > 0 {
			call, _ := profiles.CurrentState().History.Get(0)
			updateViewsWithCall(gui, call)
		}
		return nil
	})
	g.Update(setKeybindings)
}

func exitHistoryView(g *gocui.Gui) {
	g.SetManagerFunc(layout)
	g.Update(func(gui *gocui.Gui) error {
		setView(gui, CMD_LINE_VIEW, CMD_LINE)
		return nil
	})
	g.Update(setKeybindings)
}

func dumpEnvironment(name string) (output string) {
	if name == "User" || name == "" {
		for key, value := range profiles.CurrentState().Environment.User.Mapping {
			output += fmt.Sprintf("%s.%s=%s\n", profiles.CurrentState().Environment.User.Name, key, value)
		}
	}
	if name == "Var" || name == "" {
		for key, value := range profiles.CurrentState().Environment.OS.Mapping {
			output += fmt.Sprintf("%s.%s=%s\n", profiles.CurrentState().Environment.OS.Name, key, value)
		}
	}
	if name == "Home" || name == "" {
		for key, value := range profiles.CurrentState().Environment.Home.Mapping {
			output += fmt.Sprintf("%s.%s=%s\n", profiles.CurrentState().Environment.Home.Name, key, value)
		}
	}
	return output
}

var validProfileNameHelp string = `
- Must be lower case
- Each character must be within a-z or a digit`

var genericHelp string = `SYNOPSIS

The bottom view is for inputting commands, much like a shell,
albeit not quite as fancy. All commands output to the response
body view if they output anything.

Use the 'get', 'post', 'put' commands with a url to make basic
HTTP calls. The 'header KEY=VALUE' command can be used to add
headers in addition to modifying the request header section.
HTTP calls are saved to a history viewable with the the 'history'
command. OS environment variables are accessible via the
{{Var.NAME}} syntax in the header and body views and viewable
using the 'env' command. User-defined variables are displayed and
accessed using the User name via the 'env' command.

ENVIRONMENT

There are three environments within call-buddy. They are called
'Var', 'Home' and 'User'. The first is pulled from the OS
environment call-buddy is running in. The second is usually empty
unless the -e flag is used; it contains an environment pulled from a
file. When using the 'tcb' utility, this is used to store the
environment of the original launching host. The last contains
user accessible environment variables.

These variables can be accessed within the request header and
request body using the {{ENV.KEY}} syntax. For example, if
APIKEY is stored in the OS environment, putting {{Var.APIKEY}} in
the request header or request body will result in this being
expanded and sent in each HTTP request.

To display a particular environment use the 'env' command with the
name as the second argument. If no environment name is provided, all
environment variables are printed out.

To set a user-defined environment variable, use the 'env' command
with KEY=VALUE as the second argument. Quoting is not currently
supported :(.

PERSISTENCE

When using call-buddy the current state is automatically saved to a
json file in ~/.call-buddy or $XDG_HOME_DIR if defined. Consider
this directory to be solely managed by the call-buddy utility, lest
you risk unexpected errors or issues.

HISTORY

Every HTTP call made is stored internally for later access. Your
history can be accessed using the 'history' command; doing so opens
up a hidden view that you can arrow up or down in. Your selection
temporarily updates the rest of the view with the call. Hitting
enter makes the update permanent, hitting escape cancels the update.
Both cause the history view to be exited and hidden again.

PROFILES

If you wish to separate history and environment variables between
different invocations of the call-buddy command or even within the
same invocation, you can create and use particular profiles using
the 'create' and 'use' commands. All the available profiles can be
shown using the 'profiles' command. To remove or rename a profile,
the 'remove' and 'rename' commands can be used.

Profile names must be lower case and each character must be in
a-z or a digit.

COMMANDS

- help [CMD]    Outputs a detailed help message
- exit, quit    Goodbye.
- get URL       Issues a http GET request
- delete URL    Issues a http DELETE request
- put URL       Issues a http PUT request
- post URL      Issues a http POST request
- head URL      Issues a http HEAD request
- header K=V    Appends a KEY=VALUE pair to the header view
- history       Enters the history view
- env [N][K=V]  Outputs one or more named envs or stores a key
- ! SHELL       Executes the shell command and outputs it
- > FILE        (Over)writes the output to a file
- < FILE        (Over)writes the response body with a file
- >> FILE       Appends the output to a file
- profiles      Outputs the available profiles
- create NAME   Creates a new profile
- use NAME      Uses the given profile
- remove NAME   Removes (and deactivates) the given profile
- rename NAME   Renames the given profile

KEYBINDINGS

- Tab           Switch to the next view.
- F2            Switch to the command line.
- Ctrl-Y        Enter/exit the history view.
- Ctrl-E        Go to end of line (UNIX only).
- Ctrl-A        Go to start of line (UNIX only).
- Ctrl-W        Clear a word backwards (UNIX only).
- Ctrl-U        Clear a line backwards (UNIX only).

CREDITS

This application was built and developed by the Terminal Call-Buddy
Team at the University of Utah: Dylan Gardner, Alex Herrmann,
Cooper Pender and Derek Dixon. Credited for the idea goes to Alex
Herrmann.

END

Thanks for using call-buddy! :^)`

var helpSynopsises map[string]string = map[string]string{
	"exit":     "exit",
	"!":        "! SHELL-COMMAND",
	">":        "> FILE",
	"<":        "< FILE",
	">>":       ">> FILE",
	"env":      "env [KEY=VALUE]\nenv [NAME]",
	"header":   "header KEY=VALUE",
	"help":     "help [COMMAND]",
	"history":  "history",
	"profiles": "profiles",
	"create":   "create NAME",
	"remove":   "remove NAME",
	"use":      "use NAME",
	"rename":   "rename OLD-NAME NEW-NAME",
	"post":     "post URL",
	"get":      "get URL",
	"put":      "put URL",
	"delete":   "delete URL",
	"head":     "head URL",
}

// helpDescriptions A mapping between commands and their help descriptions.
var helpDescriptions map[string]string = map[string]string{
	"exit": `
Closes the application with an exit code of 0.

ALIASES
	q, quit`,
	"!": `
Passes the given shell command(s) to the shell and executes it. The
response body is piped into the shell's stdin and the shell's stdout
is captured and updated in place of the response body pane after the
shell finishes execution.

The shell is choosen and executed in the following manner:

  1. If the SHELL environment variable is set in either the {{User}}
     environment or {{Var}} environment (which is inherited from the
     spawning process' environment), with the former taking priority,
     then '{{SHELL}} -c SHELL-COMMAND' is executed. The SHELL-COMMAND
     need not to be escaped.

  2. If the OS is Windows, PowerShell is used and executed as:
     'PowerShell.exe -Command SHELL-COMMAND'. The SHELL-COMMAND need
     not to be escaped.

  3. Otherwise, a UNIX system is assumed and /bin/sh is used in the
     manner described for the SHELL environment variable.

EXAMPLES

Assuming a UNIX system.

! grep KEY              Filters the response body to only contain
                        lines with 'KEY'
! ifconfig              Dumps the network interfaces.
! tail -30 | grep KEY   Filters the response body to only contain
                        the last 30 lines and filters those lines
                        again to those that contain 'KEY'`,
	">": `
Saves the call response to the given file (and overrides the contents).`,
	">>": `
Saves and appends the call response to the given file.`,
	"<": `
Loads the given file into the call response (and overrides the contents).`,
	"env": `
Displays the environment or stores the given key value pair in the
'User' environment. Use {{User.KEY}} to extract the value.`,
	"header": `
Stores the given key value header in the request header view.`,
	"help": `
Provides help on call-buddy and on specific commands.

ALIASES
	?, man`,
	"history": `
Enters the history view.`,
	"profiles": `
Lists the available profiles.`,
	"create": `
Creates and activates a new profile with the given name. Profiles
can be removed using the 'remove' command. A valid name is based
on the following conditions:
` + validProfileNameHelp,
	"remove": `
Removes and deactivates the given profile if active. The leftmost
profile after shown in the 'profiles' command after completion is
selected. If there are no profiles after completion, a new default
profile is created.`,
	"use": `
Deactivates the current profile and activates the requested
profile.`,
	"rename": `
Renames the requested profile name. A valid name is based on the
following conditions:
` + validProfileNameHelp,
	"post": `
Issues a http POST request with the request headers and body in the
view.`,
	"get": `
Issues a http GET request.`,
	"put": `
Issues a http PUT request with the request headers and body in the
view.`,
	"delete": `
Issues a http DELETE request.`,
	"head": `
Issues a http HEAD request.`,
}

// helpMessagesOrder The order to display the help messages in since go
// randomizes iteration order. Also, not alphabetical since some are more
// important among others.
var helpMessagesOrder []string = []string{
	"help",
	"exit",
	"get",
	"post",
	"put",
	"delete",
	"head",
	"header",
	"history",
	"env",
	"!",
	">",
	">>",
	"<",
	"profiles",
	"create",
	"use",
	"remove",
	"rename",
}

// help Returns a string with help output. If a command is given in argv, the
// corresponding help message for that command is given. If the command does
// not exist, an error message is returned.
func help(argv []string) string {
	// Generic help
	if len(argv) < 2 {
		return genericHelp
	}
	command := argv[1]

	// Specific command help
	if message, found := helpDescriptions[command]; found {
		return "SYNOPSIS\n\n" + helpSynopsises[command] + "\n\nDESCRIPTION\n" + message
	}
	if argv[0] == "help" && command == "me" {
		return "Hang in there bud. :)"
	}
	return "No documentation for '" + argv[1] + "'"
}

// lookupShell Returns the shell for the user.
func lookupShell() (shellArgv []string) {
	// Normally we'd lookup the default shell via getpwuid, but that only
	// exists on UNIX systems and it gets really realy messy to deal with
	// cgo and linking against libc for all cross-compilations. So we'll
	// use $SHELL instead and then fallback to /bin/sh for non-Windows
	// systems and PowerShell for our Windows friends
	shell := profiles.CurrentState().Environment.User.Expand("{{User.SHELL}}")
	if shell != "" {
		shellArgv = []string{shell, "-c"}
		return
	}

	shell = profiles.CurrentState().Environment.OS.Expand("{{Var.SHELL}}")
	if shell != "" {
		shellArgv = []string{shell, "-c"}
		return
	}

	shellArgv = []string{"/bin/sh", "-c"}
	if runtime.GOOS == "windows" {
		shellArgv = []string{"PowerShell.exe", "-Command"}
	}
	return
}

func bang(argv []string, input string) string {
	shellArgv := lookupShell()
	shellArgv = append(shellArgv, argv[1:]...)

	cmd := exec.Command(shellArgv[0], shellArgv[1:]...)
	cmd.Stdin = strings.NewReader(input)

	// Grab out stderr and stdout
	// FIXME DG: We shouldn't trust stdout/stderr to be a valid string,
	//           but rather a byte stream so string(output) might cause
	//           problems...
	var outputBuf bytes.Buffer
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf
	err := cmd.Run()

	if err != nil {
		return outputBuf.String() + "\n" + err.Error()
	}
	return outputBuf.String()
}

func evalCmdLine(g *gocui.Gui) (err error) {
	var ourErr error // Returning an error causes panic! wtf
	var historicalCall t.HistoricalCall
	var appendToFile bool
	// FIXME: Deal with errors!
	cmdLineView, _ := g.View(CMD_LINE_VIEW)
	rspBodyView, _ := g.View(RSP_BODY_VIEW)
	rqtBodyView, _ := g.View(RQT_BODY_VIEW)
	rqtHeaderView, _ := g.View(RQT_HEAD_VIEW)
	requestBodyBuffer := rqtBodyView.Buffer()
	requestHeadersBuffer := rqtHeaderView.Buffer()

	// Extract the command into an args list
	rawCommand := strings.TrimSpace(cmdLineView.Buffer())
	if rawCommand == "" {
		return
	}
	argv := strings.Split(rawCommand, " ")
	command := strings.ToLower(argv[0])

	switch command {
	case "!":
		if len(argv) < 2 {
			message := help([]string{"help", command})
			updateResponseBodyView(rspBodyView, message)
			break
		}
		rest := strings.Join(argv[1:], " ")
		message := bang([]string{command, rest}, rspBodyView.Buffer())
		rspBodyView, _ := g.View(RSP_BODY_VIEW)
		updateResponseBodyView(rspBodyView, message)

	case "?": // Just in case people get confused
		fallthrough
	case "man": // They're _technically_ man pages
		fallthrough
	case "help":
		message := help(argv)
		updateResponseBodyView(rspBodyView, message)

	case "<":
		if len(argv) < 2 {
			message := help([]string{"help", command})
			updateResponseBodyView(rspBodyView, message)
			break
		}
		loadResponseFromFile(rqtBodyView, argv[1])

	case ">":
		appendToFile = false
		fallthrough
	case ">>":
		appendToFile = true
		if len(argv) < 2 {
			message := help([]string{"help", command})
			updateResponseBodyView(rspBodyView, message)
			break
		}
		saveResponseToFile(rspBodyView.Buffer(), argv[1], appendToFile)

	case "env":
		if len(argv) < 2 {
			env := dumpEnvironment("")
			updateResponseBodyView(rspBodyView, env)
		} else if strings.Contains(argv[1], "=") {
			for _, kv := range argv[1:] {
				addUserEnvironmentVariable(kv)
			}
		} else {
			env := dumpEnvironment(argv[1])
			updateResponseBodyView(rspBodyView, env)
		}
		updateCommandLineView(cmdLineView, "")

	case "create":
		if len(argv) < 2 {
			message := help([]string{"help", command})
			updateResponseBodyView(rspBodyView, message)
			break
		}
		_, ourErr := profiles.New(stateDir, argv[1])
		if ourErr != nil {
			updateResponseBodyView(rspBodyView, ourErr.Error())
		} else {
			updateResponseBodyView(rspBodyView, "Profile "+argv[1]+" has been created and is now active.")
		}

	case "use":
		if len(argv) < 2 {
			message := help([]string{"help", command})
			updateResponseBodyView(rspBodyView, message)
			break
		}
		_, ourErr := profiles.Use(argv[1])
		if ourErr != nil {
			updateResponseBodyView(rspBodyView, ourErr.Error())
		} else {
			updateResponseBodyView(rspBodyView, argv[1]+" is now the current profile.")
		}

	case "remove":
		if len(argv) < 2 {
			message := help([]string{"help", command})
			updateResponseBodyView(rspBodyView, message)
			break
		}

		var tempComplete string
		for _, selected := range argv[1:] {
			ourErr := profiles.Remove(stateDir, selected)
			if ourErr != nil {
				tempComplete += "Failed to remove " + selected + ": " + ourErr.Error() + "\n"
			} else {
				tempComplete += "Successfully Removed " + selected + "\n"
			}
		}
		updateResponseBodyView(rspBodyView, tempComplete)

	case "rename":
		if len(argv) < 2 {
			message := help([]string{"help", command})
			updateResponseBodyView(rspBodyView, message)
			break
		}

		ourErr := profiles.Rename(argv[1], argv[2])
		if ourErr != nil {
			updateResponseBodyView(rspBodyView, ourErr.Error())
		} else {
			updateResponseBodyView(rspBodyView, argv[1]+" is now named "+argv[2])
		}

	case "profiles":
		if len(argv) >= 2 {
			message := help([]string{"help", command})
			updateResponseBodyView(rspBodyView, message)
			break
		}

		var curList = profiles.List()
		var tempList string
		for i, selected := range curList {
			if i == 0 {
				tempList += selected.Name + " <-- Current\n"
			} else {
				tempList += selected.Name + "\n"
			}
		}
		updateResponseBodyView(rspBodyView, tempList)

	case "history":
		enterHistoryView(g)

	case "q":
		fallthrough
	case "quit":
		fallthrough
	case "exit":
		err = gocui.ErrQuit
		return

	case "header":
		if len(argv) < 2 {
			message := help([]string{"help", command})
			updateResponseBodyView(rspBodyView, message)
			break
		}
		appendHeaderToView(argv[1], rqtHeaderView)

	case "get":
		fallthrough
	case "put":
		fallthrough
	case "post":
		fallthrough
	case "delete":
		fallthrough
	case "head":
		// Assume is a call
		if len(argv) < 2 {
			updateResponseBodyView(rspBodyView, "Invalid Usage: <call-type> <url>")
			break
		}
		url := argv[1]
		if historicalCall, ourErr = call(command, url, requestBodyBuffer, requestHeadersBuffer); ourErr != nil {
			// Print error out in place of response body
			updateResponseBodyView(rspBodyView, ourErr.Error())
			return
		}
		profiles.CurrentState().History.AddFinishedCall(historicalCall)
		profiles.Save(stateDir)
		updateViewsWithCall(g, historicalCall)

	default:
		updateResponseBodyView(rspBodyView, "No such command '"+command+"'. Use help.")
	}
	return
}

func updateViewsWithCall(g *gocui.Gui, call t.HistoricalCall) {
	// Print out new response

	g.Update(func(gui *gocui.Gui) error {
		rspBodyView, _ := gui.View(RSP_BODY_VIEW)
		responseBody := call.Response.String()
		updateResponseBodyView(rspBodyView, responseBody)
		return nil
	})
	g.Update(func(gui *gocui.Gui) error {
		// Update the request views
		methodBodyView, _ := gui.View(MTD_BODY_VIEW)
		updateMethodBodyView(methodBodyView, call.Request.URL, call.Request.Method)
		return nil
	})

	g.Update(func(gui *gocui.Gui) error {
		requestHeaderView, _ := gui.View(RQT_HEAD_VIEW)
		updateRequestHeaderView(requestHeaderView, call.Request.Header)
		return nil
	})
}

func setView(gui *gocui.Gui, name string, state ViewState) {
	currView = state
	currViewPtr, _ := gui.SetCurrentView(name)
	// FIXME Dylan: This should be done only if the editor is editable
	currViewPtr.Editor = &theEditor
	gui.SetViewOnTop(name)
	gui.Cursor = true
}

func switchNextView(g *gocui.Gui, v *gocui.View) error {
	// FIXME: Properly handle errors
	// Round robben switching between views
	switch currView {
	case CMD_LINE:
		// -> method body
		setView(g, MTD_BODY_VIEW, MTD_BODY)
	case MTD_BODY:
		// -> request headers
		setView(g, RQT_HEAD_VIEW, RQT_HEAD)
	case RQT_HEAD:
		// -> request body
		setView(g, RQT_BODY_VIEW, RQT_BODY)
	case RQT_BODY:
		// -> reqponse body
		setView(g, RSP_BODY_VIEW, RSP_BODY)
	case RSP_BODY:
		// -> command line
		setView(g, CMD_LINE_VIEW, CMD_LINE)
	case HIST_BODY:
		exitHistoryView(g)
		// -> command line
		setView(g, CMD_LINE_VIEW, CMD_LINE)
	default:
		log.Panicf("Got to a unknown view! %d\n", currView)
	}
	return nil
}

func switchPrevView(g *gocui.Gui, v *gocui.View) error {
	// FIXME: Properly handle errors

	// Round robben switching between views
	switch currView {
	case CMD_LINE:
		// -> method body
		setView(g, RSP_BODY_VIEW, RSP_BODY)
	case MTD_BODY:
		// -> request headers
		setView(g, CMD_LINE_VIEW, CMD_LINE)
	case RQT_HEAD:
		// -> request body
		setView(g, MTD_BODY_VIEW, MTD_BODY)
	case RQT_BODY:
		// -> reqponse body
		setView(g, RQT_HEAD_VIEW, RQT_HEAD)
	case RSP_BODY:
		// -> command line
		setView(g, RQT_BODY_VIEW, RQT_BODY)
	case HIST_BODY:
		exitHistoryView(g)
		// -> method body
		setView(g, RSP_BODY_VIEW, RSP_BODY)
	default:
		log.Panicf("Got to a unknown view! %d\n", currView)
	}
	return nil
}

func setHistView(g *gocui.Gui, v *gocui.View) error {
	// FIXME: POSSIBLY SWITCH BACK TO LAST SELECTED VIEW?
	if currView == HIST_BODY {
		exitHistoryView(g)
		//setView(g, CMD_LINE_VIEW, CMD_LINE)
	} else {
		enterHistoryView(g)
		//setView(g, HIST_VIEW, HIST_BODY)
	}
	return nil
}

func setCommandView(g *gocui.Gui, view *gocui.View) error {
	if currView != HIST_BODY {
		setView(g, CMD_LINE_VIEW, CMD_LINE)
	} else {
		exitHistoryView(g)

	}

	return nil
}

func updateMethodBodyView(view *gocui.View, url string, method t.HttpMethod) {
	view.Clear()

	fmt.Fprintln(view, url)
	fmt.Fprintln(view)
	for _, possibleMethod := range t.AllHttpMethods() {
		x := " "
		if possibleMethod == method {
			x = "x"
		}
		fmt.Fprintf(view, "[%s] %s\n", x, possibleMethod.String())
	}
}

func updateRequestHeaderView(view *gocui.View, headers http.Header) {
	view.Clear()

	// For some reason golang stores the value as a list... I dunno why
	for key, values := range headers {
		fmt.Fprintf(view, "%s: %s\n", key, strings.Join(values, " "))
	}
}

func updateRequestBodyView(view *gocui.View, body string) {
	view.Clear()
	fmt.Fprint(view, "")
	fmt.Fprint(view, body)
}

func updateResponseBodyView(view *gocui.View, body string) {
	view.Clear()
	fmt.Fprint(view, "")
	fmt.Fprint(view, body)
}

func updateHistoryView(view *gocui.View) {
	view.Clear()
	histFormat := profiles.CurrentState().History.GetSimpleWholeHistoryReport()
	fmt.Fprint(view, histFormat)
}

func updateCommandLineView(view *gocui.View, command string) {
	view.Clear()
	fmt.Fprint(view, command)
	view.SetCursor(len(command), 0)
}

//Setting the manager
func layout(g *gocui.Gui) error {
	maxX, maxY := g.Size()
	realMaxX, realMaxY := maxX-1, maxY-1
	verticalSplitX := 32         // Defines the vertical split down to the command line
	horizontalSplitY := maxY - 4 // Defines the horizontal command line split

	// Call-Buddy Title
	titleYStart := 0
	titleYEnd := titleYStart + 2
	if v, err := g.SetView(TTL_LINE_VIEW, 0, titleYStart, verticalSplitX, titleYEnd, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		fmt.Fprint(v, "\u001b[32mTerminal "+"\u001b[29mCall "+"\u001b[29mBuddy")
	}

	// Response Body (e.g. html)
	if v, err := g.SetView(RSP_BODY_VIEW, verticalSplitX+1, titleYStart, realMaxX, horizontalSplitY, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Title = "Response Body"
		v.Wrap = true
		v.Autoscroll = false
		v.Editable = true
	}

	// Method Body (e.g. GET, PUT, HEAD...)
	methodBodyYStart := titleYEnd + 1
	methodBodyYEnd := methodBodyYStart + 10
	if v, err := g.SetView(MTD_BODY_VIEW, 0, methodBodyYStart, verticalSplitX, methodBodyYEnd, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Title = "Method Body"
		updateMethodBodyView(v, "http://", "get")
	}

	// Request Headers (e.g. Content-type: text/json)
	requestHeadersYStart := methodBodyYEnd + 1
	requestHeadersYEnd := requestHeadersYStart + 6
	if v, err := g.SetView(RQT_HEAD_VIEW, 0, requestHeadersYStart, verticalSplitX, requestHeadersYEnd, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Title = "Request Headers"
		v.Wrap = true
		v.Autoscroll = false
		v.Editable = true
	}

	// Request Body (e.g. json: {})
	requestBodyYStart := requestHeadersYEnd + 1
	if v, err := g.SetView(RQT_BODY_VIEW, 0, requestBodyYStart, verticalSplitX, horizontalSplitY, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Title = "Request Body"
		v.Wrap = true
		v.Autoscroll = false
		v.Editable = true
	}

	// Command Line (e.g. :get http://httpbin.org/get)
	if v, err := g.SetView(CMD_LINE_VIEW, 0, horizontalSplitY+1, realMaxX, realMaxY, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Wrap = false
		v.Editable = true
		v.Autoscroll = false
	}
	return nil
}

//Possible layout for when history is popped up.
func histLayout(g *gocui.Gui) error {
	maxX, maxY := g.Size()
	realMaxX, realMaxY := maxX-1, maxY-1
	verticalSplitX := 32         // Defines the vertical split down to the command line
	horizontalSplitY := maxY - 4 // Defines the horizontal command line split

	// Call-Buddy Title
	titleYStart := 0
	titleYEnd := titleYStart + 2
	if v, err := g.SetView(TTL_LINE_VIEW, 0, titleYStart, verticalSplitX, titleYEnd, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		fmt.Fprint(v, "\u001b[32mTerminal "+"\u001b[29mCall "+"\u001b[29mBuddy")
	}

	historyYEnd := titleYStart + 6
	//History View
	if v, err := g.SetView(HIST_VIEW, verticalSplitX+1, titleYStart, realMaxX, historyYEnd, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Title = "History"
		v.Wrap = false
		v.Editable = false
		v.Autoscroll = false
	}

	// Response Body (e.g. html)
	if v, err := g.SetView(RSP_BODY_VIEW, verticalSplitX+1, historyYEnd+1, realMaxX, horizontalSplitY, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Title = "Response Body"
		v.Wrap = true
		v.Autoscroll = false
		v.Editable = true
	}

	// Method Body (e.g. GET, PUT, HEAD...)
	methodBodyYStart := titleYEnd + 1
	methodBodyYEnd := methodBodyYStart + 10
	if v, err := g.SetView(MTD_BODY_VIEW, 0, methodBodyYStart, verticalSplitX, methodBodyYEnd, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Title = "Method Body"
		updateMethodBodyView(v, "http://", "get")
	}

	// Request Headers (e.g. Content-type: text/json)
	requestHeadersYStart := methodBodyYEnd + 1
	requestHeadersYEnd := requestHeadersYStart + 6
	if v, err := g.SetView(RQT_HEAD_VIEW, 0, requestHeadersYStart, verticalSplitX, requestHeadersYEnd, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Title = "Request Headers"
		v.Wrap = true
		v.Autoscroll = false
		v.Editable = true
	}

	// Request Body (e.g. json: {})
	requestBodyYStart := requestHeadersYEnd + 1
	if v, err := g.SetView(RQT_BODY_VIEW, 0, requestBodyYStart, verticalSplitX, horizontalSplitY, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Title = "Request Body"
		v.Wrap = true
		v.Autoscroll = false
		v.Editable = true
	}

	// Command Line (e.g. :get http://httpbin.org/get)
	if v, err := g.SetView(CMD_LINE_VIEW, 0, horizontalSplitY+1, realMaxX, realMaxY, 0); err != nil {
		if !gocui.IsUnknownView(err) {
			return err
		}
		v.Wrap = false
		v.Editable = true
		v.Autoscroll = false
		updateCommandLineView(v, "")
	}
	return nil
}

//This is the function to QUIT out of the TUI
func quit(g *gocui.Gui, v *gocui.View) error {
	return gocui.ErrQuit
}

func setKeybindings(g *gocui.Gui) error {

	// Global Keybindings
	if err := g.SetKeybinding("", gocui.KeyTab, gocui.ModNone, switchNextView); err != nil {
		log.Panicln(err)
	}
	if err := g.SetKeybinding("", gocui.KeyCtrlY, gocui.ModNone, setHistView); err != nil {
		log.Panicln(err)
	}
	if err := g.SetKeybinding("", gocui.KeyF2, gocui.ModNone, setCommandView); err != nil {

	}
	if err := g.SetKeybinding("", gocui.KeyEnd, gocui.ModNone, endOfLine); err != nil {
		log.Panicln(err)
	}
	if err := g.SetKeybinding("", gocui.KeyHome, gocui.ModNone, startOfLine); err != nil {
		log.Panicln(err)
	}
	if err := g.SetKeybinding("", gocui.KeyCtrlU, gocui.ModNone, clearLine); err != nil {
		log.Panicln(err)
	}

	//Unix only keybindings
	if runtime.GOOS != "windows" {
		if err := g.SetKeybinding("", gocui.KeyCtrlE, gocui.ModNone, endOfLine); err != nil {
			log.Panicln(err)
		}
		if err := g.SetKeybinding("", gocui.KeyCtrlA, gocui.ModNone, startOfLine); err != nil {
			log.Panicln(err)
		}
		if err := g.SetKeybinding("", gocui.KeyCtrlW, gocui.ModNone, clearWord); err != nil {
			log.Panicln(err)
		}
		if err := g.SetKeybinding("", gocui.KeyCtrlC, gocui.ModNone, quit); err != nil {
			log.Panicln(err)
		}
	}

	// View-Specific Keybindings:

	// Command View Keybindings
	if err := g.SetKeybinding(CMD_LINE_VIEW, gocui.KeyEnter, gocui.ModNone, cmdOnEnter); err != nil {
		log.Panicln(err)
	}

	// History View Keybindings
	if err := g.SetKeybinding(HIST_VIEW, gocui.KeyArrowDown, gocui.ModNone, histArrowDown); err != nil {
		log.Panicln(err)
	}
	if err := g.SetKeybinding(HIST_VIEW, gocui.KeyArrowUp, gocui.ModNone, histArrowUp); err != nil {
		log.Panicln(err)
	}
	if err := g.SetKeybinding(HIST_VIEW, gocui.KeyEnter, gocui.ModNone, histOnEnter); err != nil {
		log.Panicln(err)
	}
	if err := g.SetKeybinding(HIST_VIEW, gocui.KeyTab, gocui.ModNone, histOnTab); err != nil {
		log.Panicln(err)
	}

	return nil

}

func main() {
	envFile := flag.String("e", "", "Environment file to load from")
	flag.Parse()
	if envFile != nil {
		profiles.CurrentState().Environment.Home.PopulateFromFile(*envFile)
	}

	//Setting up a new TUI
	g, err := gocui.NewGui(gocui.OutputNormal, false)
	if err != nil {
		log.Panicln(err)
	}
	defer g.Close()

	theEditor = TCBEditor{g, false}
	g.Highlight = true
	g.Cursor = true
	g.SelFgColor = gocui.ColorGreen

	//Setting a manager, sets the view (defined as another function above)
	g.SetManagerFunc(layout)

	setKeybindings(g)

	currView = CMD_LINE
	g.SetCurrentView(CMD_LINE_VIEW)

	if err := g.MainLoop(); err != nil && err != gocui.ErrQuit {
		log.Panicln(err)
	}
}

// FIXME DG: This should not be a global, ideally we subclass a view and store
//           the state within that!
type viewBackup struct {
	methodBuffer        string
	requestHeaderBuffer string
	requestBodyBuffer   string
	responseBodyBuffer  string
}

// store Given a GUI, stores the relevant state of the GUI into the backup.
func (backup *viewBackup) store(g *gocui.Gui) {
	methodBodyView, _ := g.View(MTD_BODY_VIEW)
	backup.methodBuffer = methodBodyView.Buffer()

	requestHeaderView, _ := g.View(RQT_HEAD_VIEW)
	backup.requestHeaderBuffer = requestHeaderView.Buffer()

	requestBodyView, _ := g.View(RQT_BODY_VIEW)
	backup.requestBodyBuffer = requestBodyView.Buffer()

	responseBodyView, _ := g.View(RSP_BODY_VIEW)
	backup.responseBodyBuffer = responseBodyView.Buffer()
}

// restore Given a GUI, restores the stored state into the relevant parts of the GUI.
func (backup *viewBackup) restore(g *gocui.Gui) {
	methodBodyView, _ := g.View(MTD_BODY_VIEW)
	methodBodyView.Clear()
	fmt.Fprint(methodBodyView, backup.methodBuffer)

	requestHeaderView, _ := g.View(RQT_HEAD_VIEW)
	requestHeaderView.Clear()
	fmt.Fprint(requestHeaderView, backup.requestHeaderBuffer)

	requestBodyView, _ := g.View(RQT_BODY_VIEW)
	requestBodyView.Clear()
	fmt.Fprint(requestBodyView, backup.requestBodyBuffer)

	responseBodyView, _ := g.View(RSP_BODY_VIEW)
	responseBodyView.Clear()
	fmt.Fprint(responseBodyView, backup.responseBodyBuffer)
}

func (backup *viewBackup) clear() {
	backup.methodBuffer = ""
	backup.requestHeaderBuffer = ""
	backup.requestBodyBuffer = ""
	backup.responseBodyBuffer = ""
}

var histHintViewBackup viewBackup

// histOnEnter Populates the history with the currently selected history item
func histOnTab(g *gocui.Gui, v *gocui.View) error {
	histHintViewBackup.restore(g)
	return switchNextView(g, v)
}

func histArrowUp(gui *gocui.Gui, view *gocui.View) error {
	curX, curY := view.Cursor()
	if curY > 0 && profiles.CurrentState().History.Size() > 0 {
		curY -= 1

		// Show hint for selected history
		call, _ := profiles.CurrentState().History.Get(curY)
		updateViewsWithCall(gui, call)
	}
	view.SetCursor(curX, curY)
	return nil
}

func histArrowDown(gui *gocui.Gui, view *gocui.View) error {
	curX, curY := view.Cursor()
	if curY < profiles.CurrentState().History.Size()-1 {
		curY += 1

		// Show hint for selected history
		call, _ := profiles.CurrentState().History.Get(curY)
		updateViewsWithCall(gui, call)
	}
	view.SetCursor(curX, curY)
	return nil
}

// cmdOnEnter Evaluates the command line
func cmdOnEnter(g *gocui.Gui, v *gocui.View) error {
	return evalCmdLine(g)
}

//Function to handle going to the end of a command line
func endOfLine(gui *gocui.Gui, view *gocui.View) error {
	view.EditGotoToEndOfLine()
	return nil
}

//Function to handle going to the start of a command line
func startOfLine(gui *gocui.Gui, view *gocui.View) error {
	view.EditGotoToStartOfLine()
	return nil
}

//Deletes the current text in the view
func clearLine(gui *gocui.Gui, view *gocui.View) error {
	view.EditDeleteToStartOfLine()
	return nil
}

//Deletes the current word in the view
func clearWord(gui *gocui.Gui, view *gocui.View) error {
	view.EditDeleteToBeforeWord() // call-buddy specific feature of gocui
	return nil
}

// histOnEnter Populates the history with the currently selected history item
func histOnEnter(g *gocui.Gui, v *gocui.View) error {
	// Always switch back to command line view
	exitHistoryView(g)

	// We've already hinted the state when we hit up or down arrows, simply
	// clear the backup to make it permanent
	histHintViewBackup.clear()

	// Load the command corresponding to the history element at the cursor
	// into the view
	_, curY := v.Cursor()
	var cmd string
	historicalCall, err := profiles.CurrentState().History.Get(curY)
	if err != nil {
		cmd = ""
	}
	updateViewsWithCall(g, historicalCall)

	cmd = generateCommand(historicalCall)
	cmdView, _ := g.View(CMD_LINE_VIEW)
	updateCommandLineView(cmdView, cmd)

	return nil
}

func generateCommand(historicalCall t.HistoricalCall) string {
	// {method} {request URL} [content-type]
	cmd := fmt.Sprintf("%s %s", historicalCall.Request.Method, historicalCall.Request.URL)

	// Golang's net.http.Header is a map[string][]string for some reason
	if len(historicalCall.Request.Header["Content-type"]) > 0 {
		cmd += " " + historicalCall.Request.Header["Content-type"][0]
	}
	return cmd
}
