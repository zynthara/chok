// Copyright (C) 2012 Rob Figueiredo. All Rights Reserved.
// Ported from github.com/robfig/cron/v3 v3.0.1 under the MIT license; the
// license text and the changes this port makes are in the NOTICE file at
// the root of this repository.

// Package cron turns cron specs into schedules. It is the spec parser of
// github.com/robfig/cron/v3 without its job runner: package jobs runs the
// schedules itself.
//
// A spec is five space-separated fields, or six with a leading seconds
// field when the parser accepts one:
//
//	Field name   | Mandatory? | Allowed values  | Allowed special characters
//	----------   | ---------- | --------------  | --------------------------
//	Seconds      | No         | 0-59            | * / , -
//	Minutes      | Yes        | 0-59            | * / , -
//	Hours        | Yes        | 0-23            | * / , -
//	Day of month | Yes        | 1-31            | * / , - ?
//	Month        | Yes        | 1-12 or JAN-DEC | * / , -
//	Day of week  | Yes        | 0-6 or SUN-SAT  | * / , - ?
//
// Month and day-of-week names are case insensitive. A slash gives an
// increment over a range (3-59/15, or */15 over the whole field; N/15
// means N-max/15), a comma separates a list, a hyphen gives a range, and
// a question mark may stand for a star in the day-of-month or the
// day-of-week field. When both day fields are restricted, a day matching
// either one matches.
//
// The descriptors @yearly (or @annually), @monthly, @weekly, @daily (or
// @midnight) and @hourly stand for the corresponding specs, and
// "@every <duration>" activates once every duration as time.ParseDuration
// reads it, rounded to whole seconds and at least one second.
//
// A spec is read in the machine's local time zone unless it starts with
// CRON_TZ=<zone> (or TZ=<zone>) and a space: "CRON_TZ=Asia/Tokyo 0 6 * * *"
// runs at 6am in Tokyo. Activations that fall into a daylight-saving gap
// do not happen.
package cron

import "time"

// Schedule describes a job's duty cycle.
type Schedule interface {
	// Next returns the next activation time, later than the given time.
	// Next is invoked initially, and then each time the job is run.
	Next(time.Time) time.Time
}
