package signup

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Mailer is the transport of sign-up and invite email: the login service's
// login.MailSwitch in production (the transport a platform admin chose).
type Mailer interface {
	Ready() bool
	SendText(ctx context.Context, to, subject, text string) error
}

// mailJob is one queued email. Its text can hold a secret (a confirm code,
// an invite link), so a job is never logged.
type mailJob struct{ to, subject, text string }

// queue sends sign-up email on a goroutine that main owns (Run), so a
// public route answers in the same time whether or not an email goes out.
// It is bounded: when full, an email is dropped and one line is logged
// (without the address or the text).
type queue struct {
	m       Mailer
	log     *slog.Logger
	jobs    chan mailJob
	timeout time.Duration
}

func newQueue(m Mailer, log *slog.Logger, size int) *queue {
	return &queue{m: m, log: log, jobs: make(chan mailJob, size), timeout: 15 * time.Second}
}

func (q *queue) enqueue(j mailJob) {
	select {
	case q.jobs <- j:
	default:
		q.log.Error("sign-up email queue is full; an email was not sent")
	}
}

// run sends until ctx ends. Jobs still queued then are dropped.
func (q *queue) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-q.jobs:
			q.sendOne(ctx, j)
		}
	}
}

func (q *queue) sendOne(ctx context.Context, j mailJob) {
	sctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	if err := q.m.SendText(sctx, j.to, j.subject, j.text); err != nil {
		// A *login.MailError's text is safe: the step and a reply code.
		q.log.ErrorContext(ctx, "sign-up email failed", "subject", j.subject, "err", err)
	}
}

// The email texts. Plain text only. The subjects hold no secret: the dev
// log mailer writes the subject (never the body) to the log.

func confirmMail(code string) mailJob {
	return mailJob{
		subject: "Your Launchpad sign-up code",
		text: fmt.Sprintf("Your sign-up confirm code is %s\n\n"+
			"Enter it on the sign-up page to confirm this email address. It works once and expires in 1 hour.\n"+
			"An org admin still has to approve your request before you can log in.\n"+
			"If you did not ask to join, you can ignore this email.\n", code),
	}
}

// existingAccountMail answers a sign-up request for an address that has an
// account. Only the owner of the address gets it.
func existingAccountMail(base string) mailJob {
	where := "the Launchpad web app"
	if base != "" {
		where = strings.TrimRight(base, "/") + "/login"
	}
	return mailJob{
		subject: "Your Launchpad sign-up request",
		text: fmt.Sprintf("Someone asked to join Launchpad with this email address.\n\n"+
			"This address has an account already, so nothing changed. Log in at %s.\n"+
			"If you did not ask, you can ignore this email.\n", where),
	}
}

func approvedMail(base, orgName string) mailJob {
	where := "the Launchpad web app"
	if base != "" {
		where = strings.TrimRight(base, "/") + "/login"
	}
	return mailJob{
		subject: "Your Launchpad account is ready",
		text: fmt.Sprintf("An admin approved your request to join %s on Launchpad.\n\n"+
			"Log in at %s with this email address. You will get a one-time login code by email.\n", orgName, where),
	}
}

func inviteMail(inviter, orgName, link string, expires time.Time) mailJob {
	return mailJob{
		subject: "You are invited to join " + oneLine(orgName) + " on Launchpad",
		text: fmt.Sprintf("%s invited you to join %s on Launchpad.\n\n"+
			"Open this link to accept: %s\n\n"+
			"It works once and expires on %s.\n"+
			"If you did not expect this email, you can ignore it.\n",
			inviter, orgName, link, expires.UTC().Format("2 January 2006 15:04 MST")),
	}
}

// oneLine removes control characters from a header value (the SMTP mailer
// does it too; this keeps the subject the same on every transport).
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
