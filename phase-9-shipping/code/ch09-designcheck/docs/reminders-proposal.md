# Reminder emails

*An invented proposal, with faults planted in it, for the exercise in Chapter 9.9.*

## Summary

We will send our customers' customers a reminder by email when an invoice is overdue.

## Design

A job runs on the reporting server every day at 09:00. It selects every overdue invoice. For each one it calls the email provider, and then marks the invoice as reminded.

The select, the calls and the updates are in one database transaction, so that a failure leaves nothing half done. If a call to the provider fails, the job tries it again until it succeeds. If the job itself fails, it starts again from the beginning the next day.

## Load

Small.

## Timeline

Two weeks.
