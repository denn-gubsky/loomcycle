---
name: Calc/dates
kind: section
description: "Calc dates and times — date and datetime values (ISO 8601 only), date arithmetic in days and seconds, month steps that clamp to the month end, datediff and age, yearfrac day-count bases, working days with holidays, xnpv and xirr, parts, datefmt, and time zones with tz."
---
Two value kinds. A **date** is a calendar day with no zone: `date("2026-11-20")`.
A **datetime** is an instant: `datetime("2026-11-20T14:30:00Z")`; with no zone
it is UTC, and an offset (`+02:00`) is converted to UTC. Results render as ISO
8601, with `type` `date` or `datetime`; `exact` is `true` (a date is never
rounded). Years run from 1 to 9999.

- **Only ISO 8601 is read.** `11/12/2026` is refused (`bad_date`): it is
  12 November or 11 December depending on the writer's country. Write
  `2026-11-12`.
- **A date in `vars` is a string**; wrap it: `date(start)` with
  `"vars": {"start": "2026-11-20"}`.
- **Arithmetic.** `d + 30` and `d - 30` move by days (whole days only);
  `d2 - d1` is the number of days, exact. On a datetime the number is
  **seconds**: `dt + 3600` is an hour later, `dt2 - dt1` is seconds, exact to
  the nanosecond. Seconds are elapsed time: across a daylight-saving change
  `dt + 86400` reads an hour off; for calendar days use dates.
- **Compare** `== != < <= > >=` within one kind. A date and a datetime never
  mix: convert with `date(dt)` or `datetime(d)`. `* /` and `^` are refused.
- `today()` and `now()` read the server clock (once per call), so their
  answer changes from day to day. For a repeatable answer pass the date in
  `vars`.
- A variable may share a function's name: after `year = 2026`,
  `date(year, 1, 1)` reads the variable and `year(d)` still calls the
  function.

## Making dates

### date(s) or date(y, m, d)
The date of an ISO 8601 string, of year, month and day, or of a datetime (its
calendar date in its zone). A day past the month's end is refused, not rolled
over.

Example: `date(2026, 11, 20) + 41` gives `2026-12-31`.

### datetime(s)
The instant of an ISO 8601 / RFC 3339 string (`T` or a space, seconds and
fraction optional, `Z` or an offset), or midnight UTC of a date.

Example: `datetime("2026-11-20T16:30:00+02:00")` gives `2026-11-20T14:30:00Z`.

### today()
The current date in UTC.

Example: `today() - today()` gives `0`.

### now()
The current instant, to the second, in UTC.

Example: `now() - now()` gives `0`.

### tz(dt, zone)
The same instant, rendered and split into parts in an IANA zone (`"Europe/Kyiv"`,
`"America/New_York"`, `"UTC"`); an abbreviation such as `CEST` is refused.

Example: `tz(datetime("2026-11-20T14:30:00Z"), "Europe/Kyiv")` gives `2026-11-20T16:30:00+02:00`.

## Calendar steps

### addmonths(d, n)
`n` calendar months after `d` (before, for a negative `n`). A day the target
month lacks becomes its last day: 31 January plus one month is the end of
February.

Example: `addmonths(date("2026-01-31"), 1)` gives `2026-02-28`.

### addyears(d, n)
`n` years after `d`; 29 February becomes 28 February in a common year.

Example: `addyears(date("2024-02-29"), 1)` gives `2025-02-28`.

### eomonth(d, n)
The last day of the month `n` months after `d`'s month, as Excel's EOMONTH.

Example: `eomonth(date("2026-11-20"), 0)` gives `2026-11-30`.

## Differences

### datediff(a, b, unit)
Whole `"days"`, `"weeks"`, `"months"` or `"years"` from `a` to b, negative
when `a` is after `b`. A month is complete when `b`'s day of the month reaches
`a`'s (Excel's DATEDIF "M"): 31 January to 28 February is 0 months, to 1 March
is 1. Years count the same way.

Example: `datediff(date("2026-01-31"), date("2026-03-01"), "months")` gives `1`.

### age(birth, on?)
Completed years from `birth` to `on`, default today. Someone born on
29 February turns a year older on 1 March in a common year.

Example: `age(date("1990-11-21"), date("2026-11-20"))` gives `35`.

### yearfrac(a, b, basis)
The years from `a` to `b` under a day-count basis, exact; negative when `a` is
after `b`. `"act/365"`: days / 365. `"act/360"`: days / 360. `"30/360"`: US
(NASD) 30/360 as Excel's YEARFRAC basis 0, February rules included.
`"30e/360"`: European 30E/360 (basis 4). `"act/act"`: ISDA, the days in each
calendar year over that year's length — not Excel's basis 1, which averages
the year lengths.

Example: `yearfrac(date("2026-01-01"), date("2026-07-01"), "act/365")` gives `0.495890410958904`.

## Working days

Weekends are Saturday and Sunday. Holidays are a date or a list of dates you
pass; there is no built-in holiday calendar.

### workdays(a, b, holidays?)
Monday-to-Friday days from `a` to `b`, **both included**, less the holidays,
as Excel's NETWORKDAYS; negative when `a` is after `b`.

Example: `workdays(date("2026-11-02"), date("2026-11-06"))` gives `5`.

### addworkdays(d, n, holidays?)
The date `n` working days after `d` (before, for a negative `n`); `d` itself
is not counted, as Excel's WORKDAY.

Example: `addworkdays(date("2026-11-20"), 1)` gives `2026-11-23`.

## Dated cash flows

These are here, not in a finance section, because they need dates. Flows
and dates are lists of one length; the first date is the start, and no date
may precede it. Both give a float.

### xnpv(rate, flows, dates)
The net present value: each flow divided by `(1 + rate)^(days / 365)`, the
days counted from the first date (Excel's XNPV).

Example: `round(xnpv(0.1, [-1000, 1100], [date("2026-01-01"), date("2027-01-01")]), 2)` gives `0`.

### xirr(flows, dates, guess?)
The annual rate at which `xnpv` is zero (Excel's XIRR); `guess` starts the
search, default 0.1. Needs a negative and a positive flow. When no rate
zeroes the value it is refused (`domain`), never approximated.

Example: `round(xirr([-1000, 1100], [date("2026-01-01"), date("2027-01-01")]), 6)` gives `0.1`.

## Parts and tests

A part of a datetime is read in its zone: UTC, or the one `tz` gave it.

### year(d)
The year.

Example: `year(date("2026-11-20"))` gives `2026`.

### month(d)
The month, 1 to 12.

Example: `month(date("2026-11-20"))` gives `11`.

### day(d)
The day of the month.

Example: `day(date("2026-11-20"))` gives `20`.

### weekday(d)
The ISO day of the week: **1 is Monday, 7 is Sunday**.

Example: `weekday(date("2026-11-20"))` gives `5`.

### isoweek(d)
The ISO 8601 week, 1 to 53. Early January can be in the previous year's
last week.

Example: `isoweek(date("2021-01-03"))` gives `53`.

### dayofyear(d)
The day of the year, 1 to 366.

Example: `dayofyear(date("2026-11-20"))` gives `324`.

### quarter(d)
The calendar quarter, 1 to 4.

Example: `quarter(date("2026-11-20"))` gives `4`.

### daysinmonth(d)
The days in `d`'s month.

Example: `daysinmonth(date("2028-02-10"))` gives `29`.

### isleap(d)
Whether the year of a date, or a year number, is a leap year.

Example: `isleap(1900)` gives `false`.

### hour(dt)
The hour of a datetime, 0 to 23. A date has none.

Example: `hour(tz(datetime("2026-11-20T14:30:00Z"), "Asia/Tokyo"))` gives `23`.

### minute(dt)
The minute, 0 to 59.

Example: `minute(datetime("2026-11-20T14:30:00Z"))` gives `30`.

### second(dt)
The second, with its fraction.

Example: `second(datetime("2026-11-20T14:30:05.25Z"))` gives `5.25`.

## Formatting

### datefmt(d, pattern)
`d` written with these tokens: `YYYY` `YY` year, `MMMM` `MMM` month name,
`MM` `M` month number, `DD` `D` day, `dddd` `ddd` weekday name, `HH` `mm`
`ss` time. Names are English. Text in `[brackets]` is copied as is, and so
are digits, spaces and punctuation outside them. Any other letter, and `%`,
is refused (`bad_date`): a letter run must be a whole token, so `DDD` or
`MMMMM` is refused too. The `hint` names the one to write: `%A` or `EEEE` is
`dddd`, `%Y` or `yyyy` is `YYYY`, `%d` or `dd` is `DD`, the week number is
`isoweek(d)`, the day of the year `dayofyear(d)`. Write `[Week of] D MMM`,
not `Week of D MMM`, and a literal percent as `[%]`.

Example: `datefmt(date("2026-11-05"), "dddd, D MMMM YYYY")` gives `Thursday, 5 November 2026`.

## Examples

Days left to a deadline:

```json
{"expr": "date(due) - date(from)", "vars": {"due": "2026-12-31", "from": "2026-11-20"}}
```

Result: 41

A 24-month contract's last day:

```json
{"expr": "addmonths(date(\"2026-01-31\"), 24) - 1"}
```

Result: 2028-01-30

Working days in December 2026, Christmas off:

```json
{"expr": "d = date(\"2026-12-01\"); workdays(d, eomonth(d, 0), date(\"2026-12-25\"))"}
```

Result: 22

A loan of 10,000 repaid in three payments of 3,500: its annual rate.

```json
{"expr": "round(xirr([10000, -3500, -3500, -3500], map(ds, s -> date(s))), 6)", "vars": {"ds": ["2026-01-15", "2026-07-15", "2027-01-15", "2027-07-15"]}}
```

Result: 0.050353

A meeting time in New York:

```json
{"expr": "datefmt(tz(datetime(\"2026-11-20T14:30:00Z\"), \"America/New_York\"), \"ddd HH:mm\")"}
```

Result: Fri 09:30

A variable named like a date function, beside the function itself:

```json
{"expr": "year = 2026; days = date(year, 12, 31) - date(year, 11, 21); [days, year(date(\"2027-03-01\"))]"}
```

Result: [40, 2027]
