---
name: Calc/units
kind: section
description: "Calc unit conversion — convert and convertdelta between units of length, mass, time, temperature, area, volume, speed, acceleration, force, pressure, energy, power, angle, frequency and data size; the unit names, SI and binary prefixes, compound units, temperature, and what is exact."
---
Convert a number between units of the same dimension. Units are strings from
a closed table, and **case matters**: `mm` is a millimetre and `Mm` a
megametre, `b` a bit and `B` a byte. Out of scope: numbers that carry units
through arithmetic (`3 m * 2 m`) and currency.

## Functions

### convert(value, from, to)
`value` in unit `from`, written in unit `to`. `value` is a number or a list
(nested lists too); a list is converted element by element. Exact when the
value and the units' definitions are exact; an angle unit defined through pi
(`deg` to `rad`) gives a float. Refused (`dimension`) when the units measure
different things, naming both.

Example: `convert(5, "mi", "km")` gives `8.04672`.

Example: `convert(90, "km/h", "m/s")` gives `25`.

Example: `convert([32, 212], "degF", "degC")` gives `[0, 100]`.

Example: `convert(1, "kg", "m")` gives `error dimension`.

### convertdelta(value, from, to)
Like `convert`, but by scale only, with no offset: for a temperature
**difference** or rate. A rise of 18 °F is a rise of 10 °C, while 18 °F as a
reading is about -7.8 °C. Other units convert as with `convert`.

Example: `convertdelta(18, "degF", "degC")` gives `10`.

Example: `convertdelta(2, "degC/min", "K/h")` gives `120`.

### units(dimension?)
With no argument, the dimension names; with one, the unit symbols of that
dimension, without prefixed forms.

Example: `units("temperature")` gives `["K", "degC", "degF", "degR"]`.

## Units

Symbols first; long names in brackets are accepted too, without prefixes.
**P** marks a unit that takes the SI prefixes.

- **length** — `m` P (meter, metre), `in` (inch), `ft` (foot, feet), `yd`,
  `mi` (mile, international), `nmi` (nautical mile, 1852 m), `au`, `ly`, `Å`.
- **mass** — `g` P (gram; `kg` is the base), `t` (tonne), `lb` (lbs, pound),
  `oz`, `st` (stone), `ton` (US short ton, 2000 lb), `gr` (grain), `ct`
  (carat).
- **time** — `s` P (sec), `min`, `h` (hr), `d` (day), `wk`, `yr` (Julian
  year, 365.25 d).
- **temperature** — `K` P (kelvin), `degC` (°C, celsius), `degF` (°F),
  `degR` (rankine).
- **area** — any length squared (`m^2`, `ft²`), `ha`, `acre`.
- **volume** — any length cubed, `L` and `l` P (liter, litre), `gal` (US),
  `impgal` (imperial), `qt`, `pt`, `cup`, `floz`, `tbsp`, `tsp` (US).
- **speed** — any length/time (`km/h`, `m/s`), `kph`, `mph`, `kn` (knot).
- **acceleration** — `m/s^2`, `gn` (standard gravity).
- **force** — `N` P, `dyn`, `kgf`, `lbf`.
- **pressure** — `Pa` P, `bar` P, `atm`, `Torr`, `mmHg`, `psi`.
- **energy** — `J` P, `cal` P (thermochemical, 4.184 J; `kcal` is the food
  calorie), `Wh` P (`kWh`), `eV` P, `BTU` (International Table), `erg`.
- **power** — `W` P, `hp` (mechanical, 550 ft·lbf/s).
- **angle** — `rad` P, `deg` (°), `arcmin`, `arcsec`, `grad`, `turn` (rev).
- **frequency** — `Hz` P, `rpm` (a frequency: 60 rpm is 1 Hz).
- **data size** — `bit` and `b` (bits), `B` (bytes, 8 bits).
- Base units for compounds: `A`, `mol`, `cd`.

`units("length")` and the others list the same names.

## Prefixes

SI prefixes: `y z a f p n µ` (or `u`) `m c d da h k M G T P E Z Y`, from
10^-24 to 10^24. They go on the units marked P: `km`, `mg`, `µs`, `MHz`,
`hPa`, `kcal`, `kWh`, `mL`. Not on others: `kmi` and `mdegF` are refused.

Data units take only `k M G T P E Z Y` (decimal: `kB` = 1000 bytes) and the
binary `Ki Mi Gi Ti Pi Ei Zi Yi` (`KiB` = 1024 bytes). `KB` is refused:
write `kB` or `KiB`. `Mb` is a megabit, `MB` a megabyte.

## Compound units

Combine units with `*` (or `·`), `/` and `^`, with parentheses if you need
them: `"kg*m/s^2"`, `"m^3"`, `"km/h"`, `"W/(m^2*K)"`. `a/b*c` reads left to
right as `(a/b)*c`. An exponent is a whole number from -12 to 12, not 0;
`²` and `³` work too. Write `m^2`, not `m2`; `1/s` is allowed. Spaces around
the operators are ignored. A unit string is at most 64 bytes.

Two units convert when their base dimensions match, whatever they are
called: `"kg*m/s^2"` converts to `N`, `"J/s"` to `W`. Angle and data size are
dimensions of their own, so `rad` does not convert to a plain number.

## Temperature

`degC` and `degF` have an offset, so `convert` treats them as readings:
`convert(0, "degC", "degF")` is `32`. `K` and `degR` start at absolute zero.
Inside a compound (`degC/s`, `J/degF`) an offset scale is ambiguous, so
`convert` refuses it (`dimension`): write `K` or `degR` there, or use
`convertdelta`, which treats every temperature unit as a size of degree.

## Exact or not

Factors are exact where the definition is exact: the international inch
(0.0254 m), foot, yard and mile; the pound (0.45359237 kg) and ounce; the US
gallon (231 in³) and imperial gallon (4.54609 L); minute, hour, day, Julian
year; the calorie (4.184 J), standard atmosphere (101325 Pa), standard
gravity (9.80665 m/s²), electronvolt, BTU; the Celsius and Fahrenheit
offsets. Sources: NIST SP 811 Appendix B and the SI brochure. `deg`,
`arcmin`, `arcsec`, `grad` and `turn` convert exactly among themselves but
through pi to `rad`, which gives a float. `exact` in the result says which.

## Examples

A road distance and a speed:

```json
{"expr": "[convert(26.2, \"mi\", \"km\"), round(convert(100, \"km/h\", \"mph\"), 1)]"}
```

Result: [42.1648128, 62.1]

Body temperature in Celsius, exactly:

```json
{"expr": "convert(98.6, \"degF\", \"degC\")"}
```

Result: 37

A whole column of readings, passed in `vars`:

```json
{"expr": "convert(temps, \"degF\", \"degC\")", "vars": {"temps": [32, 50, 212]}}
```

Result: [0, 10, 100]

How many 4 KiB blocks a 1 GB file needs:

```json
{"expr": "ceil(convert(1, \"GB\", \"KiB\") / 4)"}
```

Result: 244141

Fuel economy from miles per gallon to litres per 100 km:

```json
{"expr": "mpg = 30; round(100 / convert(mpg, \"mi/gal\", \"km/L\"), 2)"}
```

Result: 7.84

A force is not a mass:

```json
{"expr": "convert(10, \"lb\", \"N\")"}
```

Result: error dimension
