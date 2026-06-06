"""Report courtyard bbox overlaps between placed footprints (and board edge)."""
import math

from sexpr import find, find_all
from harvest import get_footprint
from design import COMPONENTS
from pcb_positions import PCB_POS, BOARD


def courtyard_bbox(fpnode):
    xs, ys = [], []
    for t in ('fp_line', 'fp_rect', 'fp_poly', 'fp_circle', 'fp_arc'):
        for e in find_all(fpnode, t):
            lay = find(e, 'layer')
            if not lay or 'CrtYd' not in str(lay[1]):
                continue
            if t == 'fp_circle':
                c = find(e, 'center'); en = find(e, 'end')
                cx, cy = float(c[1]), float(c[2])
                r = math.hypot(float(en[1]) - cx, float(en[2]) - cy)
                xs += [cx - r, cx + r]; ys += [cy - r, cy + r]
                continue
            for k in ('start', 'end', 'mid', 'center'):
                n = find(e, k)
                if n:
                    xs.append(float(n[1])); ys.append(float(n[2]))
            pts = find(e, 'pts')
            if pts:
                for xy in find_all(pts, 'xy'):
                    xs.append(float(xy[1])); ys.append(float(xy[2]))
    if not xs:  # fall back to pads
        for pad in find_all(fpnode, 'pad'):
            at = find(pad, 'at')
            sz = find(pad, 'size')
            xs += [float(at[1]) - float(sz[1]) / 2, float(at[1]) + float(sz[1]) / 2]
            ys += [float(at[2]) - float(sz[2]) / 2, float(at[2]) + float(sz[2]) / 2]
    return min(xs), min(ys), max(xs), max(ys)


def transform(b, x, y, rot):
    x1, y1, x2, y2 = b
    corners = [(x1, y1), (x2, y1), (x2, y2), (x1, y2)]
    a = math.radians(-rot)  # KiCad rotates CCW; board y is down
    out = []
    for cx, cy in corners:
        rx = cx * math.cos(a) - cy * math.sin(a)
        ry = cx * math.sin(a) + cy * math.cos(a)
        out.append((x + rx, y + ry))
    xs = [p[0] for p in out]; ys = [p[1] for p in out]
    return min(xs), min(ys), max(xs), max(ys)


def main():
    boxes = {}
    for ref, c in COMPONENTS.items():
        lib, name = c['fp'].split(':', 1)
        fp = get_footprint(lib, name)
        x, y, rot = PCB_POS[ref]
        boxes[ref] = transform(courtyard_bbox(fp), x, y, rot)

    refs = sorted(boxes)
    found = 0
    for i, a in enumerate(refs):
        ax1, ay1, ax2, ay2 = boxes[a]
        for b in refs[i + 1:]:
            bx1, by1, bx2, by2 = boxes[b]
            ox = min(ax2, bx2) - max(ax1, bx1)
            oy = min(ay2, by2) - max(ay1, by1)
            if ox > 0.01 and oy > 0.01:
                print('OVERLAP %-5s %-5s  by (%.2f x %.2f)  %s=(%.1f..%.1f, %.1f..%.1f) %s=(%.1f..%.1f, %.1f..%.1f)'
                      % (a, b, ox, oy,
                         a, ax1, ax2, ay1, ay2, b, bx1, bx2, by1, by2))
                found += 1
    bd = BOARD
    for r in refs:
        x1, y1, x2, y2 = boxes[r]
        if x1 < bd['x1'] or y1 < bd['y1'] or x2 > bd['x2'] or y2 > bd['y2']:
            print('EDGE    %-5s  (%.1f..%.1f, %.1f..%.1f)' % (r, x1, x2, y1, y2))
    print(found, 'overlaps')


if __name__ == '__main__':
    main()
