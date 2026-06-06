# ensemble-amp — ESP32-S3 I2S Audio Amplifier

USB-C PD (20 V) → buck/LDO rails → ESP32-S3 → I2S → PCM5102A DAC → TPA3118D2 Class-D amp → stereo speakers.

**Single USB-C connector does everything**: PD negotiation runs on the CC wires while
USB 2.0 data (native ESP32-S3 USB, flashing/debug) runs on D+/D− — both at the same time.

Generated KiCad 10 project: schematic is **ERC-clean**, PCB has all footprints
**placed in functional blocks with full net assignment** — routing is left to you
(DRC shows only the expected unrouted ratsnest + silkscreen cosmetics).

## Architecture

```
USB-C ── CC1/CC2 ── HUSB238A PD sink (GPIO mode: 19.1k VSET=20V, 21k ISET=3A)
  │ VBUS ── SMAJ24A TVS ──── Q1 PMOS load switch (GATE-driven, soft start) ── VAMP rail (5…20V)
  │                                                                            ├─ TPA3118D2 PVCC/AVCC ── LC filters ── SPK L/R
  │                                                                            ├─ AP63205 buck → 5V ── AP2112K → 3.3V ── FB1 → 3V3A
  │                                                                            └─ 100k/10k divider → ESP32 ADC (IO7, rail monitor)
  └ D+/D− ── USBLC6 ESD ── ESP32-S3 native USB (flash/debug, works on any port)
ESP32-S3 ── I2S ── PCM5102A ── RC + AC coupling ── amp inputs
   └── I2C OLED · EC11 encoder · LEDs · UART header · amp control · PD_FAULT (IO14)
```

### How the PD negotiation behaves

The HUSB238A requests 20 V/3 A and walks the charger's PDO list from the top, taking the
first profile that satisfies ≥3 A — so the rail adapts to whatever you plug in:

| Charger | Rail (VAMP) | Approx. max audio power |
|---|---|---|
| 60 W+ PD | 20 V | ~2×25 W/8Ω, ~2×45 W/4Ω |
| 45 W PD | 15 V | ~2×14 W/8Ω, ~2×28 W/4Ω |
| 27–30 W PD | 9–12 V | ~2×5–9 W/8Ω |
| Plain USB port | 5 V | logic only — flashing/debug |

Firmware reads the actual rail through the 100k/10k divider on **IO7** (ADC1) — display
it on the OLED. **IO14 (PD_FAULT)** goes high if the charger can't cover the request or a
fault occurs. The amp's SDZ stays low until firmware decides the rail is adequate.

## Key design choices

- **USB-PD GPIO mode, not I2C**: in GPIO mode the HUSB238A's SDA/SCL pins permanently
  become the VSET/ISET analog straps, and sharing them with the ESP32 I2C bus would corrupt
  the strap reading at plug-in. Status is read via ADC + FAULT pin instead — robust, zero
  protocol risk. (ADDR/ORIENT floats = GPIO mode, per datasheet Table 6.)
- **Load switch**: HUSB238A GATE (open drain) drives the SUD45P03 PMOS; 100k gate pull-up,
  BZT52C10 zener clamps Vgs to −10 V (rail can be 20 V, FET Vgs(max) is ±20 V), 10 nF gives
  soft start. This matches the datasheet's GPIO-mode typical application (Fig. 4) one-to-one.
  Dead-battery attach is supported and the typical app has no other power path, so the
  switch closes at plain 5 V — the board boots when plugged into a computer. The chip's own
  OVP (120 % of requested voltage) opens the PMOS on overvoltage as a bonus protection layer.
  (Datasheet is marked *Preliminary*; GATE turn-on timing is an OTP option — verify on first
  article that VAMP comes up on a non-PD port.)
- **AP2112K-3.3 LDO** (250 mV dropout) instead of AMS1117 so the 3.3 V rail stays solid even
  in the 5 V-fallback case (buck output ≈4.7 V).
- **35 V bulk capacitors** (470 µF ×4) for the 20 V rail.
- **TPA3118D2 instead of TPA3116D2**: identical die and pinout (TI SLOS708G), but the
  thermal pad faces *down* — cooled by the PCB through the thermal-via array, no
  bolt-on heatsink. At 12 V both parts are supply-limited to the same output
  (~2×15 W @ 8 Ω, ~2×25 W @ 4 Ω). The amp symbol is in the project-local
  `ensemble.kicad_sym` library.
- **Gain**: 20 dB BTL master (R13 = 5.6 kΩ from GAIN/SLV to GND, datasheet Table).
  PCM5102A full scale is 2.1 Vrms → drives the amp to full 12 V output with margin.
- **MODSEL** tied low → classic BD modulation (matches the 10 µH + 680 nF output filter).
  AM0–AM2 low → default 400 kHz switching.
- **PLIMIT** tied to GVDD → no power limiting. Add a divider later if you want to cap output.
- **SDZ / MUTE** driven from GPIO through 100 kΩ series resistors (datasheet slew-rate
  requirement) with 100 kΩ pulldowns → amp stays off/un-muted-low until firmware says so.
- **PCM5102A** hardware-config mode: SCK grounded (internal PLL from BCK), FMT=I2S,
  FLT=normal, DEMP=off. XSMT has a 10 kΩ pulldown → DAC stays soft-muted until GPIO13 goes high.
- **Audio path**: OUT → 100 Ω + 2.2 nF RC (TPA3118 input sees clean HF) → 1 µF AC coupling →
  INP; INN AC-coupled to ground per channel (single-ended use of the differential input).
- **3V3A**: ferrite-isolated analog rail for PCM5102A AVDD/CPVDD.
- **USB data**: USBLC6 ESD protection on D+/D− (its VBUS clamp pin references the internal
  5 V rail, since connector VBUS may be 20 V). CC pulldowns are inside the HUSB238A.

## GPIO map (ESP32-S3)

| Function | GPIO | | Function | GPIO |
|---|---|---|---|---|
| I2S BCLK | IO15 | | Encoder A / B / SW | IO4 / IO5 / IO6 |
| I2S LRCK | IO16 | | Amp SDZ (high = on) | IO10 |
| I2S DOUT | IO17 | | Amp MUTE (high = mute) | IO11 |
| I2C SDA (OLED) | IO8 | | Amp FAULTZ (in, low = fault) | IO12 |
| I2C SCL (OLED) | IO9 | | DAC XSMT (high = unmute) | IO13 |
| Status LED 1 / 2 | IO47 / IO48 | | UART TX / RX (J3) | TXD0 / RXD0 |
| Spare on J3 | IO1, IO2 | | Boot / Reset buttons | IO0 / EN |
| VAMP sense (ADC1, 100k/10k) | IO7 | | PD fault (in, high = fault) | IO14 |

Power-up sequence in firmware: configure I2S → XSMT high → SDZ high → MUTE low.

## Routing guidance (the part left for you)

1. **Ground**: pour the B.Cu GND zone first (both zone outlines are already in the file,
   unfilled). Keep DAC/analog ground returns away from the amp's switching return paths.
2. **Amp**: PVCC decoupling (C36–C40) right at pins 18/19/31/32 with short, wide traces;
   bootstrap caps (C43–C46) directly at their pins; OUTx → L → film cap → terminal with
   ≥1.5 mm traces (AmpOutput net class is pre-set). Stitch the thermal-via pad to both GND planes.
3. **Buck**: keep the SW5V node (U1 pin SW → L1) tiny; input cap C4 hugging VIN/GND pins.
4. **USB**: route IO19/IO20 → USBLC6 → connector as a ~90 Ω differential pair (short here,
   so geometry is forgiving).
5. **Antenna**: the keep-out zone under the ESP32 antenna (both layers) is already placed —
   don't route or pour under it. Module antenna faces the bottom board edge.

## DRC notes (intentional)

- `unconnected_items`: the board ships unrouted by design.
- USB-C shield pads sit 0.15 mm from the board edge (connector overhangs); the project
  edge-clearance rule is set to 0.1 mm accordingly.
- Min through-hole is set to 0.2 mm for the ESP32 footprint's thermal vias.
- Silkscreen overlaps are cosmetic; nudge refdes during routing.

## Bring-up checklist

1. Populate the PD front end (U7, Q1 + gate parts, TVS) + buck/LDO. Plug into a plain USB
   port first: VAMP ≈ 5 V, +5 V/+3V3 up. Then a PD charger: VAMP should step to 20 V (or 15 V
   on a 45 W charger) about a second after attach.
2. Populate ESP32. Flash over USB-C (hold BOOT, tap RESET). Verify 3V3 ≥3.2 V during Wi-Fi TX.
3. Populate DAC; play I2S tone, scope OUTL/OUTR (2.1 Vrms max, 3 V DC bias before caps is normal **inside** the DAC — the outputs themselves are ground-centered).
4. Populate amp; SDZ low at first power, then enable with a small speaker / dummy load.

## Files

- `ensemble-amp.kicad_pro` / `.kicad_sch` / `.kicad_pcb` — open with KiCad ≥ 9
- `ensemble.kicad_sym` + `sym-lib-table` — project-local TPA3118D2 symbol
- `ensemble-amp-bom.csv` — grouped BOM (42 lines, ~85 components)
- `../generator/` — Python generators (single source of truth in `design.py` +
  `pcb_positions.py`; regenerate with `python3 gen_sch.py ../ensemble-amp/ensemble-amp.kicad_sch`
  and `python3 gen_pcb.py ../ensemble-amp/ensemble-amp.kicad_pcb`)

> ⚠ Generated design — review schematic and datasheet-critical values before ordering boards.
