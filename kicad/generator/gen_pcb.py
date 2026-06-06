"""Generate the .kicad_pcb from design.py.

Footprints are copied from the installed KiCad libraries, placed in functional
blocks, and their pads are bound to the same netlist the schematic uses.
Routing is intentionally left to the user; two GND zones and the board
outline are included.
"""
import uuid

from sexpr import parse, dump, find, find_all, Sym
from harvest import get_footprint
from gen_sch import uid, ROOT_UUID, get_sym
from harvest import symbol_pins
from design import COMPONENTS, PROJECT, TITLE
from pcb_positions import PCB_POS, BOARD, ANTENNA_KEEPOUT

import gen_sch

for _ref, _pos in PCB_POS.items():
    COMPONENTS[_ref]['pcb'] = _pos
_missing = set(COMPONENTS) - set(PCB_POS)
if _missing:
    print('WARN: no explicit PCB position for:', sorted(_missing))


def collect_nets():
    """Stable net-number assignment across the whole design."""
    nets = set()
    for ref, c in COMPONENTS.items():
        nets.update(c['conn'].values())
    return {n: i + 1 for i, n in enumerate(sorted(nets))}


def pad_nets_for(ref, c):
    """pad number -> net, resolved through the symbol pin map."""
    sym = get_sym(*c['sym'])
    pins = symbol_pins(sym)
    return gen_sch.resolve_conn(pins, c['conn'], ref)


def _rot_node(node, key, extra):
    """Add `extra` degrees to the angle of every (key ... (at x y [a]) ...)."""
    for sub in find_all(node, key):
        at = find(sub, 'at')
        if at is None:
            continue
        if len(at) > 3:
            at[3] = Sym('%g' % ((float(at[3]) + extra) % 360))
        elif extra:
            at.append(Sym('%g' % (extra % 360)))


def place_footprint(ref, c, netnum):
    lib, name = c['fp'].split(':', 1)
    node = get_footprint(lib, name)
    node[1] = c['fp']  # board files use "Lib:Name"
    x, y, rot = c['pcb']

    # strip library-file-only tokens
    node[:] = [n for n in node
               if not (isinstance(n, list) and n and n[0] in
                       ('version', 'generator', 'generator_version'))]

    # insert uuid / at / path right after the layer token
    ins = 2
    for i, n in enumerate(node):
        if isinstance(n, list) and n and n[0] == 'layer':
            ins = i + 1
            break
    at = [Sym('at'), Sym('%g' % x), Sym('%g' % y)]
    if rot:
        at.append(Sym('%g' % rot))
    node.insert(ins, [Sym('path'), '/' + uid('sym', ref)])
    node.insert(ins, at)
    node.insert(ins, [Sym('uuid'), uid('fp', ref)])

    # reference / value text
    for p in find_all(node, 'property'):
        if p[1] == 'Reference':
            p[2] = ref
        elif p[1] == 'Value':
            p[2] = c['val']

    # rotation is stored as total rotation on pads and texts
    if rot:
        _rot_node(node, 'pad', rot)

    # zones inside board footprints use absolute board coordinates
    import math as _m
    a = _m.radians(-rot)
    ca, sa = _m.cos(a), _m.sin(a)
    for zn in find_all(node, 'zone'):
        for poly in find_all(zn, 'polygon') + find_all(zn, 'filled_polygon'):
            pts = find(poly, 'pts')
            if not pts:
                continue
            for xy in find_all(pts, 'xy'):
                px, py = float(xy[1]), float(xy[2])
                xy[1] = Sym('%g' % (x + px * ca - py * sa))
                xy[2] = Sym('%g' % (y + px * sa + py * ca))

    # net assignment
    padnets = pad_nets_for(ref, c)
    seen = set()
    for pad in find_all(node, 'pad'):
        num = pad[1]
        if not num:
            continue  # NPTH / mechanical
        net = padnets.get(str(num))
        if net is None:
            continue
        seen.add(str(num))
        # insert (net N "name") before (uuid) if present, else append
        pad.append([Sym('net'), Sym(str(netnum[net])), net])
    missing = set(padnets) - seen
    if missing:
        print('WARN %s: conn pins with no matching pad in %s: %s'
              % (ref, c['fp'], sorted(missing)))
    return node


def zone(net, netnum, layer, b):
    return '''(zone
  (net %d) (net_name "%s") (layer "%s") (uuid "%s")
  (name "GND_%s") (hatch edge 0.508)
  (connect_pads (clearance 0.5))
  (min_thickness 0.25) (filled_areas_thickness no)
  (fill yes (thermal_gap 0.5) (thermal_bridge_width 0.5))
  (polygon (pts (xy %g %g) (xy %g %g) (xy %g %g) (xy %g %g)))
)''' % (netnum[net], net, layer, uid('zone', layer), layer,
        b['x1'], b['y1'], b['x2'], b['y1'], b['x2'], b['y2'], b['x1'], b['y2'])


def keepout_zone():
    k = ANTENNA_KEEPOUT
    return '''(zone
  (net 0) (net_name "") (layers "F.Cu" "B.Cu") (uuid "%s")
  (name "antenna_keepout") (hatch full 0.508)
  (connect_pads (clearance 0))
  (min_thickness 0.25)
  (keepout (tracks not_allowed) (vias not_allowed) (pads allowed)
    (copperpour not_allowed) (footprints allowed))
  (fill (thermal_gap 0.5) (thermal_bridge_width 0.5))
  (polygon (pts (xy %g %g) (xy %g %g) (xy %g %g) (xy %g %g)))
)''' % (uid('zone', 'antkeepout'),
        k['x1'], k['y1'], k['x2'], k['y1'], k['x2'], k['y2'], k['x1'], k['y2'])


def main():
    netnum = collect_nets()
    b = BOARD

    fps = []
    for ref, c in COMPONENTS.items():
        fps.append(dump(place_footprint(ref, c, netnum), 1))

    nets_txt = '\n'.join('  (net %d "%s")' % (i, n)
                         for n, i in sorted(netnum.items(), key=lambda kv: kv[1]))

    outline = ('(gr_rect (start %g %g) (end %g %g) '
               '(stroke (width 0.15) (type default)) (fill no) '
               '(layer "Edge.Cuts") (uuid "%s"))'
               % (b['x1'], b['y1'], b['x2'], b['y2'], uid('outline')))

    title = ('(gr_text "%s" (at %g %g 0) (layer "F.SilkS") (uuid "%s") '
             '(effects (font (size 1.5 1.5) (thickness 0.3))))'
             % ('ensemble-amp rev A', (b['x1'] + b['x2']) / 2, b['y2'] - 3, uid('titletext')))

    out = '''(kicad_pcb
  (version 20241229)
  (generator "pcbnew")
  (generator_version "9.0")
  (general (thickness 1.6) (legacy_teardrops no))
  (paper "A4")
  (layers
    (0 "F.Cu" signal)
    (2 "B.Cu" signal)
    (9 "F.Adhes" user "F.Adhesive")
    (11 "B.Adhes" user "B.Adhesive")
    (13 "F.Paste" user)
    (15 "B.Paste" user)
    (5 "F.SilkS" user "F.Silkscreen")
    (7 "B.SilkS" user "B.Silkscreen")
    (1 "F.Mask" user)
    (3 "B.Mask" user)
    (17 "Dwgs.User" user "User.Drawings")
    (19 "Cmts.User" user "User.Comments")
    (21 "Eco1.User" user "User.Eco1")
    (23 "Eco2.User" user "User.Eco2")
    (25 "Edge.Cuts" user)
    (27 "Margin" user)
    (31 "F.CrtYd" user "F.Courtyard")
    (29 "B.CrtYd" user "B.Courtyard")
    (35 "F.Fab" user)
    (33 "B.Fab" user)
  )
  (setup
    (pad_to_mask_clearance 0)
    (allow_soldermask_bridges_in_footprints no)
    (grid_origin %g %g)
  )
  (net 0 "")
%s
%s
%s
%s
%s
%s
%s
  (embedded_fonts no)
)
''' % (b['x1'], b['y1'], nets_txt, '\n'.join(fps), outline, title,
       zone('GND', netnum, 'F.Cu', b), zone('GND', netnum, 'B.Cu', b),
       keepout_zone())
    return out


if __name__ == '__main__':
    import sys
    text = main()
    path = sys.argv[1] if len(sys.argv) > 1 else 'out.kicad_pcb'
    with open(path, 'w') as f:
        f.write(text)
    print('wrote', path, len(text), 'bytes')
