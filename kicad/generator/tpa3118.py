"""Custom KiCad symbol for the TPA3118D2 (DAP, HTSSOP-32, pad down).

Pinout verified against TI datasheet SLOS708G (TPA3116D2/TPA3118D2/TPA3130D2),
"Pin Functions" table, page 4.
"""
from sexpr import parse

# (number, name, electrical type), left side top->bottom
LEFT = [
    ('1', 'MODSEL', 'input'),
    ('2', 'SDZ', 'input'),
    ('12', 'MUTE', 'input'),
    ('3', 'FAULTZ', 'open_collector'),
    ('8', 'GAIN/SLV', 'input'),
    ('6', 'PLIMIT', 'input'),
    ('7', 'GVDD', 'power_out'),
    ('16', 'SYNC', 'bidirectional'),
    ('13', 'AM2', 'input'),
    ('14', 'AM1', 'input'),
    ('15', 'AM0', 'input'),
    ('4', 'RINP', 'input'),
    ('5', 'RINN', 'input'),
    ('10', 'LINP', 'input'),
    ('11', 'LINN', 'input'),
    ('9', 'GND', 'power_in'),
]
RIGHT = [
    ('17', 'AVCC', 'power_in'),
    ('18', 'PVCC', 'power_in'),
    ('19', 'PVCC', 'power_in'),
    ('31', 'PVCC', 'power_in'),
    ('32', 'PVCC', 'power_in'),
    ('24', 'BSPL', 'passive'),
    ('23', 'OUTPL', 'output'),
    ('21', 'OUTNL', 'output'),
    ('20', 'BSNL', 'passive'),
    ('30', 'BSPR', 'passive'),
    ('29', 'OUTPR', 'output'),
    ('27', 'OUTNR', 'output'),
    ('26', 'BSNR', 'passive'),
    ('22', 'GND', 'power_in'),
    ('25', 'GND', 'power_in'),
    ('28', 'GND', 'power_in'),
    ('33', 'PAD', 'power_in'),
]


def _pin(num, name, etype, x, y, angle):
    return ('(pin %s line (at %g %g %d) (length 5.08) '
            '(name "%s" (effects (font (size 1.27 1.27)))) '
            '(number "%s" (effects (font (size 1.27 1.27)))))'
            % (etype, x, y, angle, name, num))


def build():
    """Return parsed s-expr node for the TPA3118D2 symbol (name not lib-prefixed)."""
    pins = []
    y = 20.32
    for num, name, et in LEFT:
        pins.append(_pin(num, name, et, -20.32, y, 0))
        y -= 2.54
    y = 20.32
    for num, name, et in RIGHT:
        pins.append(_pin(num, name, et, 20.32, y, 180))
        y -= 2.54
    text = '''(symbol "TPA3118D2"
  (pin_names (offset 1.016))
  (exclude_from_sim no) (in_bom yes) (on_board yes)
  (property "Reference" "U" (at -15.24 24.13 0) (effects (font (size 1.27 1.27)) (justify left)))
  (property "Value" "TPA3118D2" (at 15.24 24.13 0) (effects (font (size 1.27 1.27)) (justify right)))
  (property "Footprint" "" (at 0 0 0) (effects (font (size 1.27 1.27)) hide))
  (property "Datasheet" "https://www.ti.com/lit/ds/symlink/tpa3118d2.pdf" (at 0 0 0) (effects (font (size 1.27 1.27)) hide))
  (property "Description" "30W stereo Class-D audio amplifier, 4.5-26V, analog in, pad-down HTSSOP-32" (at 0 0 0) (effects (font (size 1.27 1.27)) hide))
  (symbol "TPA3118D2_0_1"
    (rectangle (start -15.24 22.86) (end 15.24 -22.86)
      (stroke (width 0.254) (type default)) (fill (type background)))
  )
  (symbol "TPA3118D2_1_1"
    %s
  )
)''' % '\n    '.join(pins)
    return parse(text)[0]
