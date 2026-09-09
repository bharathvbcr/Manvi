//! Existing X11 keymap only. No clipboard, layout switch, or global remapping.
use desktop_core::{DesktopError, Result};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) struct Stroke {
    pub code: u8,
    pub modifier: Option<u8>,
}

pub(super) fn plan(
    first: u8,
    per: u8,
    symbols: &[u32],
    modifiers: &[u8],
    text: &str,
    replace: bool,
) -> Result<Vec<Stroke>> {
    let fail = || {
        DesktopError::new(
            "keyboard_mapping_unavailable",
            "Text cannot be represented safely by the current X11 keyboard mapping",
        )
    };
    if per < 2
        || symbols.is_empty()
        || !symbols.len().is_multiple_of(usize::from(per))
        || modifiers.is_empty()
        || !modifiers.len().is_multiple_of(8)
        || text.chars().count() > 256
        || text.chars().any(char::is_control)
    {
        return Err(fail());
    }
    let group = modifiers.len() / 8;
    let find = |symbol: u32, modifier_group: Option<usize>| -> Option<u8> {
        symbols
            .chunks_exact(usize::from(per))
            .enumerate()
            .find_map(|(i, row)| {
                let code = u8::try_from(usize::from(first) + i).ok()?;
                if row[0] != symbol {
                    return None;
                }
                if let Some(index) = modifier_group
                    && !modifiers[index * group..(index + 1) * group].contains(&code)
                {
                    return None;
                }
                Some(code)
            })
    };
    let shift = find(0xffe1, Some(0))
        .or_else(|| find(0xffe2, Some(0)))
        .ok_or_else(fail)?;
    let control = find(0xffe3, Some(2))
        .or_else(|| find(0xffe4, Some(2)))
        .ok_or_else(fail)?;
    let mut result = Vec::new();
    if replace {
        result.push(Stroke {
            code: find(u32::from('a'), None)
                .filter(|code| !modifiers.contains(code))
                .ok_or_else(fail)?,
            modifier: Some(control),
        });
    }
    for c in text.chars() {
        let symbol = if u32::from(c) <= 0xff {
            u32::from(c)
        } else {
            0x01000000 | u32::from(c)
        };
        let stroke = symbols
            .chunks_exact(usize::from(per))
            .enumerate()
            .find_map(|(i, row)| {
                let code = u8::try_from(usize::from(first) + i).ok()?;
                if modifiers.contains(&code) {
                    return None;
                }
                if row[0] == symbol {
                    Some(Stroke {
                        code,
                        modifier: None,
                    })
                } else if row[1] == symbol {
                    Some(Stroke {
                        code,
                        modifier: Some(shift),
                    })
                } else {
                    None
                }
            })
            .ok_or_else(fail)?;
        result.push(stroke);
    }
    // Replacing with empty text still has to delete the selection.
    if replace && text.is_empty() {
        result.push(Stroke {
            code: find(0xff08, None).ok_or_else(fail)?,
            modifier: None,
        });
    }
    Ok(result)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn refuses_unmapped_text_before_dispatch_and_uses_only_existing_modifiers() {
        let map = [
            0xffe1, 0xffe1, 0xffe3, 0xffe3, 97, 65, 49, 33, 0xff08, 0xff08,
        ];
        let modifiers = [8, 0, 9, 0, 0, 0, 0, 0];
        assert_eq!(
            plan(8, 2, &map, &modifiers, "A1", true).unwrap(),
            vec![
                Stroke {
                    code: 10,
                    modifier: Some(9)
                },
                Stroke {
                    code: 10,
                    modifier: Some(8)
                },
                Stroke {
                    code: 11,
                    modifier: None
                }
            ]
        );
        for text in ["a\n", "💳", "\0"] {
            assert!(plan(8, 2, &map, &modifiers, text, true).is_err());
        }
        assert!(plan(8, 2, &map, &[0; 8], "a", true).is_err());
        assert!(plan(8, 2, &map, &modifiers, &"a".repeat(257), true).is_err());
        assert_eq!(plan(8, 2, &map, &modifiers, "", true).unwrap().len(), 2);
    }
}
