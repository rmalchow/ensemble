"""Harvest symbol and footprint definitions from installed KiCad libraries."""
import os
import functools

from sexpr import parse, find, find_all, Sym

SYM_DIR = '/usr/share/kicad/symbols'
FP_DIR = '/usr/share/kicad/footprints'


@functools.lru_cache(maxsize=None)
def _load_symlib(libname):
    with open(os.path.join(SYM_DIR, libname + '.kicad_sym')) as f:
        root = parse(f.read())[0]
    out = {}
    for node in find_all(root, 'symbol'):
        out[node[1]] = node
    return out


def _clone(node):
    if isinstance(node, list):
        return [_clone(x) for x in node]
    return node


def get_symbol(lib, name):
    """Return a flattened copy of symbol `name` from library `lib`.

    Derived symbols ((extends "Parent")) are flattened: the parent's body is
    used, sub-unit names are renamed, and the child's properties override.
    """
    table = _load_symlib(lib)
    node = _clone(table[name])
    ext = find(node, 'extends')
    if ext is None:
        return node
    parent = get_symbol(lib, ext[1])  # recursive: parents may extend too
    merged = _clone(parent)
    merged[1] = name
    # rename sub-units Parent_x_y -> Name_x_y
    pname = parent[1]
    for sub in find_all(merged, 'symbol'):
        if sub[1].startswith(pname + '_'):
            sub[1] = name + sub[1][len(pname):]
    # child properties override parent's wholesale (positions, effects, value)
    child_props = {p[1]: p for p in find_all(node, 'property')}
    for i, p in enumerate(merged):
        if isinstance(p, list) and p and p[0] == 'property' and p[1] in child_props:
            merged[i] = _clone(child_props[p[1]])
    return merged


def symbol_pins(symnode):
    """Return list of dicts: number, name, x, y, angle, type for all pins."""
    pins = []
    for sub in find_all(symnode, 'symbol'):
        for pin in find_all(sub, 'pin'):
            at = find(pin, 'at')
            name = find(pin, 'name')
            num = find(pin, 'number')
            pins.append({
                'number': str(num[1]),
                'name': str(name[1]),
                'x': float(at[1]), 'y': float(at[2]),
                'angle': float(at[3]) if len(at) > 3 else 0.0,
                'type': str(pin[1]),
            })
    return pins


@functools.lru_cache(maxsize=None)
def get_footprint_text(lib, name):
    with open(os.path.join(FP_DIR, lib + '.pretty', name + '.kicad_mod')) as f:
        return f.read()


def get_footprint(lib, name):
    return parse(get_footprint_text(lib, name))[0]
