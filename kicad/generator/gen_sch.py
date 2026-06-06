"""Generate the .kicad_sch from design.py.

Style: every connected pin gets either a power symbol (GND below / rail above)
or a short wire stub ending in a global label. Unconnected pins get
no-connect markers. PWR_FLAGs are auto-added to nets that contain power-input
pins but no power-output pin.
"""
import math
import uuid

from sexpr import parse, dump, find, find_all, Sym
from harvest import get_symbol, symbol_pins
import tpa3118
from design import COMPONENTS, PROJECT, TITLE, PWR_SYMBOL_NETS

NS = uuid.UUID('7f9a2d00-1111-4a8e-9c1e-ensemble0001'.replace('ensemble0001', '000000000001'))
ROOT_UUID = str(uuid.uuid5(NS, 'root-sheet'))

STUB = 2.54


def uid(*parts):
    return str(uuid.uuid5(NS, '|'.join(str(p) for p in parts)))


def get_sym(lib, name):
    if lib == 'ensemble':
        assert name == 'TPA3118D2'
        return tpa3118.build()
    return get_symbol(lib, name)


def resolve_conn(pins, conn, ref):
    """Map every pin (by number) to a net or None. conn keys: number or name."""
    out = {}
    used = set()
    bynum = {p['number']: p for p in pins}
    for key, net in conn.items():
        if key in bynum:
            out[bynum[key]['number']] = net
            used.add(key)
            continue
        matches = [p for p in pins if p['name'] == key]
        if not matches:
            raise SystemExit('%s: conn key %r matches no pin (pins: %s)'
                             % (ref, key, [(p['number'], p['name']) for p in pins]))
        for p in matches:
            out[p['number']] = net
        used.add(key)
    return out


def fmt(v):
    out = ('%.4f' % v).rstrip('0').rstrip('.')
    return out if out else '0'


def prop(name, value, x, y, hide=False, justify=None):
    eff = '(effects (font (size 1.27 1.27))%s%s)' % (
        (' (justify %s)' % justify) if justify else '',
        ' (hide yes)' if hide else '')
    return '(property "%s" "%s" (at %s %s 0) %s)' % (
        name, value.replace('"', "'"), fmt(x), fmt(y), eff)


def main():
    lib_symbols = {}     # "Lib:Name" -> node
    body = []            # text chunks: symbol instances, wires, labels, ncs
    net_pin_types = {}   # net -> set of electrical types
    pwr_count = [0]

    def ensure_lib(lib, name):
        key = '%s:%s' % (lib, name)
        if key not in lib_symbols:
            node = get_sym(lib, name)
            node[1] = key
            lib_symbols[key] = node
        return key

    def power_symbol(net, x, y):
        """Place a power-net symbol whose pin is at (x, y)."""
        libid = PWR_SYMBOL_NETS[net]
        ensure_lib(*libid.split(':'))
        pwr_count[0] += 1
        ref = '#PWR%03d' % pwr_count[0]
        u = uid('pwr', ref, x, y)
        body.append('''(symbol (lib_id "%s") (at %s %s 0) (unit 1)
  (exclude_from_sim no) (in_bom yes) (on_board yes) (dnp no)
  (uuid "%s")
  %s
  %s
  %s
  %s
  (pin "1" (uuid "%s"))
  (instances (project "%s" (path "/%s" (reference "%s") (unit 1))))
)''' % (libid, fmt(x), fmt(y), u,
        prop('Reference', ref, x, y + 6, hide=True),
        prop('Value', net, x, y + (2.8 if net == 'GND' else -3.2)),
        prop('Footprint', '', x, y, hide=True),
        prop('Datasheet', '', x, y, hide=True),
        uid('pwrpin', ref), PROJECT, ROOT_UUID, ref))

    def glabel(net, x, y, angle):
        body.append('(global_label "%s" (shape input) (at %s %s %d) '
                    '(effects (font (size 1.27 1.27)) (justify %s)) (uuid "%s"))'
                    % (net, fmt(x), fmt(y), angle,
                       'left' if angle in (0, 90) else 'right', uid('gl', net, x, y)))

    def wire(x1, y1, x2, y2):
        body.append('(wire (pts (xy %s %s) (xy %s %s)) '
                    '(stroke (width 0) (type default)) (uuid "%s"))'
                    % (fmt(x1), fmt(y1), fmt(x2), fmt(y2), uid('w', x1, y1, x2, y2)))

    # ------------------------------------------------ components
    for ref, c in COMPONENTS.items():
        lib, name = c['sym']
        key = ensure_lib(lib, name)
        pins = symbol_pins(lib_symbols[key])
        netmap = resolve_conn(pins, c['conn'], ref)
        ix, iy = c['sch']
        u = uid('sym', ref)

        pin_uuids = []
        for p in sorted(pins, key=lambda p: p['number']):
            pin_uuids.append('(pin "%s" (uuid "%s"))' % (p['number'], uid('pin', ref, p['number'])))

        body.append('''(symbol (lib_id "%s") (at %s %s 0) (unit 1)
  (exclude_from_sim no) (in_bom yes) (on_board yes) (dnp no)
  (uuid "%s")
  %s
  %s
  %s
  %s
  %s
  (instances (project "%s" (path "/%s" (reference "%s") (unit 1))))
)''' % (key, fmt(ix), fmt(iy), u,
        prop('Reference', ref, ix, iy - 2.54 if len(pins) <= 5 else iy - 26),
        prop('Value', c['val'], ix, iy + 2.54 if len(pins) <= 5 else iy - 23.5),
        prop('Footprint', c['fp'], ix, iy, hide=True),
        prop('Datasheet', '', ix, iy, hide=True),
        '\n  '.join(pin_uuids), PROJECT, ROOT_UUID, ref))

        # connection artifacts per pin
        for p in pins:
            tx, ty = ix + p['x'], iy - p['y']
            net = netmap.get(p['number'])
            if net is None:
                body.append('(no_connect (at %s %s) (uuid "%s"))'
                            % (fmt(tx), fmt(ty), uid('nc', ref, p['number'])))
                continue
            net_pin_types.setdefault(net, set()).add(p['type'])
            a = math.radians(p['angle'])
            ox, oy = -math.cos(a), math.sin(a)  # outward dir in schematic coords
            if net in PWR_SYMBOL_NETS and net == 'GND' and (ox, oy) == (0, 1):
                power_symbol(net, tx, ty)
            elif net in PWR_SYMBOL_NETS and net != 'GND' and (ox, oy) == (0, -1):
                power_symbol(net, tx, ty)
            else:
                ex, ey = tx + STUB * ox, ty + STUB * oy
                wire(tx, ty, ex, ey)
                if ox > 0.5:
                    ang = 0
                elif ox < -0.5:
                    ang = 180
                elif oy < -0.5:
                    ang = 90
                else:
                    ang = 270
                glabel(net, ex, ey, ang)

    # ------------------------------------------------ auto PWR_FLAGs
    driving = {'power_out', 'output', 'tri_state', 'open_collector', 'open_emitter'}
    flagged = []
    for net, types in sorted(net_pin_types.items()):
        if 'power_in' in types and not (types & driving):
            flagged.append(net)
    fx, fy = 38.1, 243.84
    for net in flagged:
        power_symbol_net = net
        ensure_lib('power', 'PWR_FLAG')
        pwr_count[0] += 1
        ref = '#FLG%03d' % pwr_count[0]
        body.append('''(symbol (lib_id "power:PWR_FLAG") (at %s %s 0) (unit 1)
  (exclude_from_sim no) (in_bom yes) (on_board yes) (dnp no)
  (uuid "%s")
  %s
  %s
  %s
  %s
  (pin "1" (uuid "%s"))
  (instances (project "%s" (path "/%s" (reference "%s") (unit 1))))
)''' % (fmt(fx), fmt(fy), uid('flg', net),
        prop('Reference', ref, fx, fy - 5, hide=True),
        prop('Value', 'PWR_FLAG', fx, fy - 3),
        prop('Footprint', '', fx, fy, hide=True),
        prop('Datasheet', '', fx, fy, hide=True),
        uid('flgpin', net), PROJECT, ROOT_UUID, ref))
        wire(fx, fy, fx, fy + 5.08)
        glabel(net, fx, fy + 5.08, 270)
        fx += 15.24

    # ------------------------------------------------ assemble
    libsyms = '\n'.join(dump(node, 1) for node in lib_symbols.values())
    out = '''(kicad_sch
  (version 20250610)
  (generator "eeschema")
  (generator_version "10.0")
  (uuid "%s")
  (paper "A3")
  (title_block
    (title "%s")
    (date "2026-06-06")
    (rev "A")
    (comment 1 "Generated design - review before fabrication")
  )
  (lib_symbols
%s
  )
%s
  (sheet_instances
    (path "/" (page "1"))
  )
  (embedded_fonts no)
)
''' % (ROOT_UUID, TITLE, libsyms, '\n'.join(body))
    return out


def write_project_lib(outdir):
    """Project-local symbol lib holding the custom TPA3118D2 + sym-lib-table."""
    import os
    node = tpa3118.build()
    lib = ('(kicad_symbol_lib\n  (version 20241209)\n  (generator "kicad_symbol_editor")\n'
           '  (generator_version "10.0")\n' + dump(node, 1) + '\n)\n')
    with open(os.path.join(outdir, 'ensemble.kicad_sym'), 'w') as f:
        f.write(lib)
    with open(os.path.join(outdir, 'sym-lib-table'), 'w') as f:
        f.write('(sym_lib_table\n  (version 7)\n'
                '  (lib (name "ensemble")(type "KiCad")'
                '(uri "${KIPRJMOD}/ensemble.kicad_sym")(options "")'
                '(descr "ensemble-amp project symbols"))\n)\n')


if __name__ == '__main__':
    import sys, os
    text = main()
    path = sys.argv[1] if len(sys.argv) > 1 else 'out.kicad_sch'
    with open(path, 'w') as f:
        f.write(text)
    write_project_lib(os.path.dirname(os.path.abspath(path)))
    print('wrote', path, len(text), 'bytes')
