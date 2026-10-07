package appadmin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// mbin creates an administrator through Mbin's own console commands, run
// in process by a PHP script piped to the app container on stdin.
//
// The commands are the fork's, read at tag v1.13.3+paisans:
//
//   - mbin:user:create <username> <email> <password>
//     (src/Command/UserCommand.php:35-37). It pre-approves the account when no
//     applicationText is given (:80) and marks it verified unconditionally
//     (:90).
//   - mbin:user:verify <username> --activate (src/Command/VerifyCommand.php:34-35,
//     :52-54).
//   - mbin:user:admin <username> (src/Command/AdminCommand.php:33, :49).
//   - mbin:user:password <username> <password>
//     (src/Command/UserPasswordCommand.php:35-36).
//
// Both commands that take a password take it only as a required positional
// argument: neither defines interact() or asks a question, so there is no
// prompt to answer on stdin. Running `bin/console mbin:user:create u e pw`
// would put the password in the process table of the host and the container
// for as long as it ran, which is the one thing this command must not do.
//
// So the toolkit does what bin/console does, minus argv. A short PHP script
// goes to `php` on stdin, boots the same App\Kernel bin/console boots
// (bin/console at the same tag), wraps it in the same FrameworkBundle
// Application, and runs each command with an ArrayInput. The password exists
// only in that script, which travels inside the ssh session's stdin and the
// docker exec stream, and in the PHP process's memory.
//
// Rejected: reimplementing the commands against UserManager directly. It
// would avoid the console layer, but it would copy the fork's logic (pre
// approval, verification, the role array) into this toolkit, where it would
// drift the first time the fork changed it. Running the commands themselves
// keeps the fork the owner of what an admin is.
type mbin struct{}

// mbinProbeMarker prefixes the probe's one line of output, so a deprecation
// notice or a warning printed while the kernel boots cannot be mistaken for
// the answer.
const mbinProbeMarker = "paisans-admin-probe "

// mbinCommand is the command line every Mbin step uses. It carries no input:
// everything variable is in the script on stdin.
//
// `docker compose exec` skips the image's entrypoint and runs as root, so the
// script is run through gosu as MBIN_USER, exactly as the entrypoint runs the
// server (docker/docker-entrypoint.sh:64 at the fork's tag). Booting the kernel
// as root could write cache files under var/ that the server, running as
// MBIN_USER, then cannot replace. MBIN_USER is set in the rendered .env.
func mbinCommand(app string) string {
	return fmt.Sprintf("docker compose -f %s exec -T app sh -c %s",
		shellQuote("/srv/"+app+"/compose.yaml"), shellQuote(`exec gosu "$MBIN_USER" php`))
}

// mbinPrelude decodes the payload and boots the kernel. The payload is JSON,
// base64 encoded, so no value needs PHP string escaping and none can end the
// literal it sits in. Exception arguments are dropped from traces so that a
// failure deep in Doctrine cannot print the password it was handed.
const mbinPrelude = `<?php
ini_set('zend.exception_ignore_args', '1');
$in = json_decode(base64_decode('%s'), true, 8, JSON_THROW_ON_ERROR);
require '/app/vendor/autoload.php';
(new Symfony\Component\Dotenv\Dotenv())->bootEnv('/app/.env');
$kernel = new App\Kernel($_SERVER['APP_ENV'], (bool) $_SERVER['APP_DEBUG']);
`

// The probe looks the user up through UserRepository::findOneByUsername, the
// same lookup every command above uses (src/Repository/UserRepository.php:267),
// and reads isVerified (src/Entity/User.php:188) and isAdmin() (:723). It
// flushes nothing.
const mbinProbe = `$kernel->boot();
$user = $kernel->getContainer()->get('doctrine')->getRepository(App\Entity\User::class)->findOneByUsername($in['username']);
echo '` + mbinProbeMarker + `', json_encode([
    'exists' => null !== $user,
    'verified' => null !== $user && $user->isVerified,
    'admin' => null !== $user && $user->isAdmin(),
]), "\n";
`

const mbinRun = `$app = new Symfony\Bundle\FrameworkBundle\Console\Application($kernel);
$app->setAutoExit(false);
$out = new Symfony\Component\Console\Output\ConsoleOutput();
foreach ($in['commands'] as $c) {
    $code = $app->run(new Symfony\Component\Console\Input\ArrayInput($c), $out);
    if (0 !== $code) {
        fwrite(STDERR, 'paisans: ' . $c['command'] . ' exited ' . $code . "\n");
        exit(1);
    }
}
`

func (mbin) Probe(t Transport, req Request) (State, error) {
	script, _, err := mbinScript(map[string]any{"username": req.Username}, mbinProbe)
	if err != nil {
		return State{}, err
	}
	out, err := t.RunInput(mbinCommand(req.App), script)
	if err != nil {
		return State{}, fmt.Errorf("probing %s for user %s: %w", req.App, req.Username, err)
	}
	return parseMbinProbe(out)
}

func parseMbinProbe(out string) (State, error) {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), strings.TrimSpace(mbinProbeMarker))
		if !ok {
			continue
		}
		var got struct {
			Exists   bool `json:"exists"`
			Verified bool `json:"verified"`
			Admin    bool `json:"admin"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &got); err != nil {
			return State{}, fmt.Errorf("reading the probe's answer %q: %w", rest, err)
		}
		return State{Exists: got.Exists, Verified: got.Verified, Admin: got.Admin}, nil
	}
	return State{}, fmt.Errorf("the probe printed no answer. It said:\n%s", out)
}

func (mbin) Execute(t Transport, req Request, actions []Action) (Outcome, error) {
	return Outcome{}, mbinExecute(t, req, actions)
}

func mbinExecute(t Transport, req Request, actions []Action) error {
	var commands []map[string]any
	for _, a := range actions {
		c := map[string]any{"--no-interaction": true, "username": req.Username}
		switch a {
		case ActionCreate:
			c["command"] = "mbin:user:create"
			c["email"] = req.Email
			c["password"] = req.Password
		case ActionVerify:
			c["command"] = "mbin:user:verify"
			c["--activate"] = true
		case ActionGrantAdmin:
			c["command"] = "mbin:user:admin"
		case ActionResetPassword:
			c["command"] = "mbin:user:password"
			c["password"] = req.Password
		default:
			return fmt.Errorf("mbin has no command for %q", a)
		}
		commands = append(commands, c)
	}
	script, payload, err := mbinScript(map[string]any{"commands": commands}, mbinRun)
	if err != nil {
		return err
	}
	if _, err := t.RunInput(mbinCommand(req.App), script); err != nil {
		return redact(fmt.Errorf("running Mbin's user commands in %s: %w", req.App, err), req.Password, payload)
	}
	return nil
}

// mbinScript returns the script and the encoded payload inside it, which the
// caller redacts from any error alongside the password.
func mbinScript(input map[string]any, body string) (script, payload string, err error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return "", "", err
	}
	payload = base64.StdEncoding.EncodeToString(raw)
	return fmt.Sprintf(mbinPrelude, payload) + body, payload, nil
}

// shellQuote wraps a value in single quotes for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
