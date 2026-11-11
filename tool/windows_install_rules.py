"""Read generated CMake directory-copy policies without executing install code.

The File API retains complete source/destination paths but omits directory filters.
Only a provable generated rule may defer a host's helper-presence check to another
component. Unsupported or ambiguous policy syntax never grants that deferral.
"""
from __future__ import annotations

import os
from pathlib import Path
import re


class UnknownRule(ValueError):
    pass


class Argument(str):
    def __new__(cls, value: str, quoted: bool):
        result = super().__new__(cls, value)
        result.quoted = quoted
        return result


def commands(text: str):
    """Tokenize CMake commands, retaining scalar quoting for condition semantics."""
    position = 0
    length = len(text)

    def bracket(start):
        match = re.match(r"\[(=*)\[", text[start:])
        if not match:
            return None
        begin = start + len(match[0])
        end = text.find("]" + match[1] + "]", begin)
        if end < 0:
            raise UnknownRule("Unterminated CMake bracket argument")
        value = text[begin:end]
        if value.startswith("\n"):
            value = value[1:]
        return value, end + len(match[1]) + 2

    while position < length:
        if text[position].isspace():
            position += 1
        elif text[position] == "#":
            block = bracket(position + 1)
            position = block[1] if block else text.find("\n", position)
            if position < 0:
                return
        else:
            match = re.match(r"([A-Za-z_][A-Za-z_0-9]*)\s*\(", text[position:])
            if not match:
                raise UnknownRule("Unsupported generated CMake command")
            name = match[1].lower()
            position += len(match[0])
            arguments = []
            while True:
                while position < length and text[position].isspace():
                    position += 1
                if position >= length:
                    raise UnknownRule("Unterminated generated CMake command")
                if text[position] == ")":
                    position += 1
                    break
                if text[position] == "#":
                    block = bracket(position + 1)
                    end = text.find("\n", position)
                    position = block[1] if block else (end if end >= 0 else length)
                    continue
                block = bracket(position)
                if block:
                    value, position = block
                    quoted = True
                else:
                    quoted = text[position] == '"'
                    if quoted:
                        position += 1
                    value = ""
                    while position < length:
                        char = text[position]
                        if quoted and char == '"':
                            position += 1
                            break
                        if not quoted and (char.isspace() or char in "()"):
                            break
                        position += 1
                        if char == "\\":
                            if position == length:
                                raise UnknownRule("Unterminated CMake escape")
                            char = text[position]
                            position += 1
                            char = {"n": "\n", "r": "\r", "t": "\t"}.get(char, char)
                        value += char
                    else:
                        raise UnknownRule("Unterminated CMake argument")
                    if not quoted and not value:
                        raise UnknownRule("Unsupported CMake condition")
                arguments.append(Argument(value, quoted))
            yield name, arguments


def matches(pattern: str, value: str) -> bool:
    # KWSys and Python agree on this ordinary ERE subset. Do not infer ownership
    # from Python-only extensions, backreferences or unsupported POSIX classes.
    if "(?" in pattern or "[:" in pattern or re.search(r"\\[A-Za-z0-9]", pattern):
        raise UnknownRule("Unsupported generated CMake regular expression")
    try:
        return re.search(pattern, value, re.IGNORECASE if os.name == "nt" else 0) is not None
    except re.error as error:
        raise UnknownRule("Invalid generated CMake regular expression") from error


def condition(arguments: list[Argument], component: str, configuration: str) -> bool | None:
    """Evaluate complete generated conditions; unknown expressions stay unknown."""
    if any(argument.quoted for argument in arguments[:2]):
        return None
    if arguments[:2] == ["CMAKE_INSTALL_COMPONENT", "STREQUAL"]:
        if len(arguments) < 3 or not arguments[2].quoted or "${" in arguments[2]:
            return None
        if len(arguments) == 3:
            return arguments[2] == component
        if (len(arguments) == 6 and arguments[3:] == ["OR", "NOT", "CMAKE_INSTALL_COMPONENT"]
                and not any(argument.quoted for argument in arguments[3:])):
            return arguments[2] == component or not component
        return None
    if arguments[:2] == ["CMAKE_INSTALL_CONFIG_NAME", "MATCHES"]:
        if len(arguments) == 3 and arguments[2].quoted and "${" not in arguments[2]:
            return matches(arguments[2], configuration)
        return None
    if len(arguments) == 1 and not arguments[0].quoted:
        if arguments[0].upper() in ("TRUE", "1"):
            return True
        if arguments[0].upper() in ("FALSE", "0"):
            return False
    return None


def permits(arguments: list[str], origin: Path, core: Path) -> bool:
    include = []
    exclude = []
    for index, argument in enumerate(arguments):
        if argument != "REGEX":
            continue
        if index + 1 >= len(arguments):
            raise UnknownRule("Missing directory filter")
        pattern = arguments[index + 1]
        (exclude if index + 2 < len(arguments) and arguments[index + 2] == "EXCLUDE" else include).append(pattern)
    if not core.as_posix().startswith(origin.as_posix().rstrip("/") + "/"):
        raise UnknownRule("Helper is outside the literal copied tree")
    # Filters apply to entries in the copied tree, never to ancestors above it.
    paths = [core.as_posix()]
    for parent in core.parents:
        paths.append(parent.as_posix())
        if parent == origin:
            break
    if any(matches(pattern, path) for pattern in exclude for path in paths):
        return False
    return "FILES_MATCHING" not in arguments or any(matches(pattern, core.as_posix()) for pattern in include)


def directory_copies_helper(script: Path, component: str, configuration: str,
                            origin: Path, core: Path, *, destination: Path, prefix: str) -> bool:
    """Prove the known source/filter/destination rule is active in this component."""
    try:
        # Each frame also remembers whether an earlier branch could have run.
        # An unknown condition prevents every later branch from granting proof.
        stack: list[tuple[str, bool, bool]] = []
        scopes = {"foreach", "while", "function", "macro", "block"}
        for name, arguments in commands(script.read_text(encoding="utf-8")):
            if name in ("if", "elseif"):
                taken = False
                if name == "elseif":
                    if not stack or stack[-1][0] != "if":
                        return False
                    _, _, taken = stack.pop()
                decision = condition(arguments, component, configuration)
                stack.append(("if", not taken and decision is True, taken or decision is not False))
            elif name == "else":
                if not stack or stack[-1][0] != "if":
                    return False
                _, _, taken = stack.pop()
                stack.append(("if", not taken, True))
            elif name == "endif":
                if not stack or stack[-1][0] != "if":
                    return False
                stack.pop()
            elif name in scopes:
                stack.append((name, False, True))
            elif name.startswith("end") and name[3:] in scopes:
                if not stack or stack[-1][0] != name[3:]:
                    return False
                stack.pop()
            elif name == "file" and all(active for _, active, _ in stack) and arguments[:1] == ["INSTALL"]:
                if not all(key in arguments for key in ("TYPE", "FILES", "DESTINATION")):
                    continue
                if arguments[arguments.index("TYPE") + 1] != "DIRECTORY":
                    continue
                placed = arguments[arguments.index("DESTINATION") + 1]
                placed = placed.replace("${CMAKE_INSTALL_PREFIX}", prefix)
                if "${" in placed or "$ENV{" in placed or Path(placed).resolve().as_posix() != destination.as_posix():
                    continue
                start = arguments.index("FILES") + 1
                sources = []
                for value in arguments[start:]:
                    if not Path(value).is_absolute():
                        break
                    sources.append(Path(value).resolve().as_posix())
                if origin.as_posix() in sources and permits(arguments, origin, core):
                    return True
        return False
    except (OSError, ValueError, IndexError):
        return False
