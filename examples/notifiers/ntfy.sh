#!/bin/sh
# Copyright Michael Robertson 2026
# SPDX-License-Identifier: Apache-2.0
#
# The "easy pattern" demonstrated (doc 17 M10-a): a complete ntfy.sh
# notifier in three lines of shell. Point a Notifier's template at this
# script (env: NTFY_TOPIC=<your topic>) and every failure pages your phone.
# The transport is YOUR business — impd only execs; swap curl+ntfy for
# sendmail, a webhook, or ft's `ft alert` without touching impd.
curl -sf \
    -H "Title: imp: $IMP_NOTIFY_REASON $IMP_NOTIFY_KIND/$IMP_NOTIFY_NAME" \
    -d "$IMP_NOTIFY_MESSAGE (exit=$IMP_NOTIFY_EXIT_CODE restarts=$IMP_NOTIFY_RESTARTS since=$IMP_NOTIFY_SINCE) — check: impctl logs $IMP_NOTIFY_PROC" \
    "https://ntfy.sh/$NTFY_TOPIC"
