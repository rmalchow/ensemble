"""Minimal s-expression parser/serializer for KiCad files."""


class Sym(str):
    """A bare (unquoted) s-expression atom."""
    __slots__ = ()


def parse(text):
    """Parse s-expression text into nested lists of Sym/str/list."""
    i, n = 0, len(text)
    stack = [[]]
    while i < n:
        c = text[i]
        if c.isspace():
            i += 1
        elif c == '(':
            new = []
            stack[-1].append(new)
            stack.append(new)
            i += 1
        elif c == ')':
            stack.pop()
            i += 1
        elif c == '"':
            j = i + 1
            buf = []
            while j < n:
                if text[j] == '\\' and j + 1 < n:
                    buf.append(text[j:j + 2])
                    j += 2
                elif text[j] == '"':
                    break
                else:
                    buf.append(text[j])
                    j += 1
            stack[-1].append(''.join(buf).replace('\\"', '"').replace('\\\\', '\\').replace('\\n', '\n'))
            i = j + 1
        else:
            j = i
            while j < n and not text[j].isspace() and text[j] not in '()':
                j += 1
            stack[-1].append(Sym(text[i:j]))
            i = j
    return stack[0]


def _escape(s):
    return s.replace('\\', '\\\\').replace('"', '\\"').replace('\n', '\\n')


def dump(node, indent=0):
    """Serialize a parsed node back to KiCad-style s-expression text."""
    if isinstance(node, Sym):
        return str(node)
    if isinstance(node, str):
        return '"%s"' % _escape(node)
    if isinstance(node, (int,)):
        return str(node)
    if isinstance(node, float):
        out = ('%.6f' % node).rstrip('0').rstrip('.')
        return out if out not in ('', '-') else '0'
    # list
    pad = '\t' * indent
    # short lists with no sublists go on one line
    if not any(isinstance(x, list) for x in node):
        return '(' + ' '.join(dump(x) for x in node) + ')'
    parts = ['(' ]
    first = True
    line = []
    for x in node:
        if isinstance(x, list):
            break
        line.append(dump(x))
    head_len = len(line)
    parts = ['(' + ' '.join(line)]
    for x in node[head_len:]:
        parts.append('\n' + '\t' * (indent + 1) + dump(x, indent + 1))
    parts.append('\n' + pad + ')')
    return ''.join(parts)


def find(node, key):
    """First child list whose head atom == key."""
    for x in node:
        if isinstance(x, list) and x and x[0] == key:
            return x
    return None


def find_all(node, key):
    return [x for x in node if isinstance(x, list) and x and x[0] == key]
