# Finding your power costs

The scaler compares machines in money, so it needs **what one more kWh costs
you right now**: the *marginal, all-in price per kWh*. This page shows how to
find it and how to check you got it right.

## 1. Find your rate plan

Your bill names the plan or tariff: "TOU-D-5-8PM", "Economy 7", "Agile",
"E-1", "flat rate", and so on. It also says:

- **whether the price depends on the time of day** (time-of-use, TOU) and
  which hours are peak, off-peak, super off-peak;
- **whether it changes by season**: many US plans have summer
  (June–September) and winter prices;
- **whether you're on a dynamic (hourly/half-hourly) tariff** that follows
  the wholesale market;
- **who supplies your generation**: in some places a separate supplier or
  community-choice program bills the generation part.

Your utility's website usually has a rate page for your plan with the current
per-kWh prices for every period.

## 2. Work out the all-in price per kWh

A bill splits each kWh into several lines. Add up **every charge that is
billed per kWh** for the period:

| Typical per-kWh lines (add these) | Not per kWh (leave out) |
|---|---|
| delivery / distribution / transmission | fixed daily or monthly charges |
| generation / supply / energy | "base services" or standing charges |
| public purpose, nuclear decommissioning, transition charges | baseline or tier *credits* (see below) |
| wildfire / bond recovery charges | one-off adjustments and climate credits |
| per-kWh taxes and fees | |

**Worked example (a US time-of-use plan, winter):**

| Period | Delivery | Generation | Other per-kWh charges* | **All-in** |
|---|---|---|---|---|
| 5–8pm | 0.31423 | 0.23392 | 0.02312 | **0.5713** |
| 8pm–8am | 0.26678 | 0.09843 | 0.02312 | **0.3883** |
| 8am–5pm | 0.24584 | 0.06555 | 0.02312 | **0.3345** |

\* e.g. 0.00573 public-purpose charges + 0.00591 wildfire fund + 0.01118 bond
recovery + 0.0003 state tax.

That becomes:

```yaml
energy:
  timeZone: America/Los_Angeles
  tariff:
    defaultPrice: "0.3883"     # 8pm-8am
    windows:
      - {start: "17:00", end: "20:00", price: "0.5713"}
      - {start: "08:00", end: "17:00", price: "0.3345"}
```

Add `months: [6,7,8,9]` (and `days: [Mon, Tue, Wed, Thu, Fri]` where weekday
and weekend prices differ) for seasonal plans. The first matching window wins,
so list the most specific windows first.
See [tou-smart-plugs.yaml](examples/tou-smart-plugs.yaml) for a full seasonal
plan.

**Tiers and baseline credits.** Some plans charge less (or give a credit) for
the first N kWh each month. The scaler cares about the *next* kWh, which is
usually above that allowance, so leave the credit out unless your use is
normally below it.

**Flat rate.** Divide your total per-kWh charges (not the whole bill) by the
kWh billed, or add up the per-kWh lines:
`tariff: {defaultPrice: "0.16"}`.

## 3. Dynamic prices

If your price changes every hour or half hour (Octopus Agile, Tibber, Nord
Pool spot contracts, Amber, ComEd hourly pricing, ...), you very likely
already have, or can add, a Home Assistant integration that exposes the
**current price** as a sensor. Point the scaler at it and keep a tariff as
the fallback:

```yaml
spec:
  integrations:
    homeAssistantURL: http://homeassistant.home:8123
    homeAssistantTokenSecretRef: {name: home-assistant}
  energy:
    price:
      homeAssistant: {entity: sensor.electricity_price}
      scale: "0.01"           # if the sensor is in cents/pence per kWh
    tariff:
      defaultPrice: "0.25"    # used whenever the live price can't be read
```

Also check whether the integration's price **includes taxes, network fees and
your supplier's margin**. Spot prices usually don't. Add the missing per-kWh
amounts, either in Home Assistant (a template sensor) or in PromQL if you use
a `prometheus` price.

The price can also come from Prometheus (`price: {prometheus: {query: ...}}`)
or any JSON endpoint (`price: {http: {url: ..., field: ...}}`).

## 4. Rate databases

In the US, the [OpenEI Utility Rate Database](https://openei.org/wiki/Utility_Rate_Database)
has most utilities' tariffs in machine-readable form. It's a good way to see
the TOU periods for your plan. Check its prices against your bill, though:
they may be out of date or exclude some per-kWh charges.

## 5. Check it against a real bill

Most utilities let you download your meter's interval data: "Green Button",
"Download my data", or a smart-meter CSV with 15-, 30- or 60-minute readings.

1. Take one billing period's interval data.
2. Multiply each interval's kWh by your configured price for that time.
3. Compare the total with the bill's energy charges (before fixed charges and
   credits).

If you're within a few percent, your tariff is right. If not, you're probably
missing a per-kWh line, have the wrong season boundaries, or your plan
changed. With solar, use *net* kWh per interval (import minus export) if
you're on net metering.

Rates change, often once or twice a year. Re-check after each change notice.

## 6. Watch it live

```sh
kubectl get powerpolicy default -o jsonpath='{.status.price} {.status.effectivePrice}{"\n"}'
```

`price` is the import price now; `effectivePrice` is what 100 extra watts
cost now, after solar surplus and the battery (see
[solar and batteries](solar-and-batteries.md)). The same numbers are exported
as the metrics `nodenap_price_per_kwh` and
`nodenap_effective_price_per_kwh`.
