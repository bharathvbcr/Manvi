use crate::{
    Action, ActionKind, Bounds, DesktopError, MAX_IMAGE_PIXELS, Node, Result, Screenshot, Window,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use image::{ImageFormat, ImageReader, Limits, RgbaImage};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::io::Cursor;

const MAX_TEMPLATE_BYTES: usize = 96 * 1024;
const MAX_CANDIDATES: u64 = 1_000_000;
const MAX_COMPARISONS: u64 = 64_000_000;

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct PixelRect {
    pub x: u32,
    pub y: u32,
    pub width: u32,
    pub height: u32,
}

impl PixelRect {
    fn inside(self, width: u32, height: u32) -> bool {
        self.width > 0
            && self.height > 0
            && self.x.checked_add(self.width).is_some_and(|v| v <= width)
            && self.y.checked_add(self.height).is_some_and(|v| v <= height)
    }
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PixelPoint {
    pub x: u32,
    pub y: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct VisualAnchor {
    pub png_base64: String,
    pub sha256: String,
    pub width: u32,
    pub height: u32,
    pub frame_width: u32,
    pub frame_height: u32,
    pub window_width: f64,
    pub window_height: f64,
    pub scale: f64,
    pub search: PixelRect,
    pub click: PixelPoint,
}

impl VisualAnchor {
    pub fn validate(&self) -> Result<()> {
        if !(8..=128).contains(&self.width)
            || !(8..=128).contains(&self.height)
            || self.png_base64.is_empty()
            || self.png_base64.len() > MAX_TEMPLATE_BYTES.div_ceil(3) * 4
            || self.sha256.len() != 64
            || !self
                .sha256
                .bytes()
                .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
            || !self.window_width.is_finite()
            || self.window_width <= 0.0
            || !self.window_height.is_finite()
            || self.window_height <= 0.0
            || !self.scale.is_finite()
            || self.scale <= 0.0
            || self.scale > 8.0
            || self.frame_width == 0
            || self.frame_height == 0
            || u64::from(self.frame_width) * u64::from(self.frame_height) > MAX_IMAGE_PIXELS as u64
            || !self.search.inside(self.frame_width, self.frame_height)
            || self.search.width < self.width
            || self.search.height < self.height
            || self.click.x >= self.width
            || self.click.y >= self.height
        {
            return Err(failure(
                "visual_invalid",
                "Invalid or oversized visual anchor, geometry, digest, or click offset",
            ));
        }
        let candidates = u64::from(self.search.width - self.width + 1)
            * u64::from(self.search.height - self.height + 1);
        if candidates > MAX_CANDIDATES {
            return Err(failure(
                "visual_search_limit",
                "Visual search exceeds one million candidate placements",
            ));
        }
        Ok(())
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct VisualMatch {
    pub target_id: String,
    pub anchor_sha256: String,
    // Private broker/worker binding. Public responses omit the raw frame digest;
    // hosts may publish a digest computed from their sanitized observation.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub frame_sha256: String,
    pub matched: PixelRect,
    pub screen_bounds: Bounds,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum ResolvedTarget {
    Semantic { node: Node },
    Visual { visual: VisualMatch },
}
impl ResolvedTarget {
    pub fn id(&self) -> &str {
        match self {
            Self::Semantic { node } => &node.id,
            Self::Visual { visual } => &visual.target_id,
        }
    }
}

fn failure(code: &str, message: &str) -> DesktopError {
    DesktopError::new(code, message)
}

fn decode_png(
    encoded: &str,
    width: u32,
    height: u32,
    max_bytes: usize,
) -> Result<(Vec<u8>, RgbaImage)> {
    if encoded.len() > max_bytes.div_ceil(3) * 4 {
        return Err(failure("image_limit", "Encoded PNG exceeds limit"));
    }
    let bytes = STANDARD
        .decode(encoded)
        .map_err(|_| failure("visual_invalid", "PNG base64 is invalid"))?;
    if bytes.len() > max_bytes {
        return Err(failure("image_limit", "PNG exceeds byte limit"));
    }
    let mut reader = ImageReader::with_format(Cursor::new(&bytes), ImageFormat::Png);
    let mut limits = Limits::default();
    limits.max_image_width = Some(width);
    limits.max_image_height = Some(height);
    limits.max_alloc =
        Some((u64::from(width) * u64::from(height) * 16).saturating_add(1024 * 1024));
    reader.limits(limits);
    let image = reader
        .decode()
        .map_err(|_| {
            failure(
                "visual_invalid",
                "PNG could not be decoded within its declared dimensions",
            )
        })?
        .into_rgba8();
    if image.width() != width || image.height() != height {
        return Err(failure(
            "visual_invalid",
            "PNG dimensions differ from the declared frame or template",
        ));
    }
    Ok((bytes, image))
}

fn matched_frame(
    window: &Window,
    capture: &Screenshot,
    anchor: &VisualAnchor,
) -> Result<(PixelRect, String)> {
    anchor.validate()?;
    if !window.bounds.valid()
        || capture.mime_type != "image/png"
        || capture.width != anchor.frame_width
        || capture.height != anchor.frame_height
        || window.bounds.width != anchor.window_width
        || window.bounds.height != anchor.window_height
        || window.scale != anchor.scale
    {
        return Err(failure(
            "visual_geometry_changed",
            "Visual anchor frame, window dimensions, or scale changed",
        ));
    }
    let (template_bytes, template) = decode_png(
        &anchor.png_base64,
        anchor.width,
        anchor.height,
        MAX_TEMPLATE_BYTES,
    )?;
    if format!("{:x}", Sha256::digest(&template_bytes)) != anchor.sha256 {
        return Err(failure(
            "visual_invalid",
            "Visual anchor PNG digest differs",
        ));
    }
    let first = template.get_pixel(0, 0);
    if template.pixels().any(|p| p[3] != 255) || template.pixels().all(|p| p == first) {
        return Err(failure(
            "visual_invalid",
            "A visual anchor must be opaque and nonuniform",
        ));
    }
    let (_, frame) = decode_png(
        &capture.base64,
        capture.width,
        capture.height,
        12 * 1024 * 1024,
    )?;
    let frame_hash = format!("{:x}", Sha256::digest(frame.as_raw()));
    let mut found = None;
    let mut compared = 0u64;
    for y in anchor.search.y..=anchor.search.y + anchor.search.height - anchor.height {
        for x in anchor.search.x..=anchor.search.x + anchor.search.width - anchor.width {
            let mut equal = true;
            'pixels: for py in 0..anchor.height {
                for px in 0..anchor.width {
                    compared += 1;
                    if compared > MAX_COMPARISONS {
                        return Err(failure(
                            "visual_search_limit",
                            "Visual search exhausted its pixel comparison budget",
                        ));
                    }
                    if frame.get_pixel(x + px, y + py) != template.get_pixel(px, py) {
                        equal = false;
                        break 'pixels;
                    }
                }
            }
            if equal {
                if found.is_some() {
                    return Err(failure(
                        "target_ambiguous",
                        "More than one exact visual anchor occurs in the declared search region",
                    ));
                }
                found = Some(PixelRect {
                    x,
                    y,
                    width: anchor.width,
                    height: anchor.height,
                });
            }
        }
    }
    Ok((
        found.ok_or_else(|| {
            failure(
                "target_missing",
                "No exact visual anchor occurs in the declared search region",
            )
        })?,
        frame_hash,
    ))
}

fn screen_bounds(window: &Window, anchor: &VisualAnchor, matched: PixelRect) -> Bounds {
    let sx = window.bounds.width / f64::from(anchor.frame_width);
    let sy = window.bounds.height / f64::from(anchor.frame_height);
    Bounds {
        x: window.bounds.x + f64::from(matched.x) * sx,
        y: window.bounds.y + f64::from(matched.y) * sy,
        width: f64::from(matched.width) * sx,
        height: f64::from(matched.height) * sy,
    }
}

pub fn resolve_visual(
    window: &Window,
    capture: &Screenshot,
    anchor: &VisualAnchor,
    observation_id: &str,
) -> Result<VisualMatch> {
    if observation_id.is_empty() || observation_id.len() > 160 {
        return Err(failure(
            "invalid_request",
            "Visual resolution requires a bounded observation identity",
        ));
    }
    let (matched, frame_sha256) = matched_frame(window, capture, anchor)?;
    let binding = serde_json::to_vec(&(observation_id, window, anchor, matched, &frame_sha256))
        .map_err(|e| DesktopError::new("serialization", e.to_string()))?;
    Ok(VisualMatch {
        target_id: format!("visual-{:x}", Sha256::digest(binding)),
        anchor_sha256: anchor.sha256.clone(),
        frame_sha256,
        matched,
        screen_bounds: screen_bounds(window, anchor, matched),
    })
}

// Fresh pixels, exact match position and the reviewed full frame must all agree.
// Platform adapters must still verify foreground ownership and the native hit
// recipient immediately before dispatch. No AX node is fabricated for a match.
pub fn revalidate_visual(
    window: &Window,
    capture: &Screenshot,
    anchor: &VisualAnchor,
    expected: &VisualMatch,
    action: &Action,
) -> Result<(f64, f64)> {
    action.validate()?;
    if !matches!(action.kind, ActionKind::Click)
        || action.x.is_some()
        || action.y.is_some()
        || action.text.is_some()
        || action.scroll_y.is_some()
        || action.target_id != expected.target_id
        || expected.anchor_sha256 != anchor.sha256
    {
        return Err(failure(
            "invalid_action",
            "Visual targets accept only their bound click offset",
        ));
    }
    let (matched, frame_hash) = matched_frame(window, capture, anchor)?;
    if matched != expected.matched
        || !screen_bounds(window, anchor, matched).near(expected.screen_bounds, 0.0)
        || frame_hash != expected.frame_sha256
    {
        return Err(failure(
            "visual_state_changed",
            "Scoped pixels or anchor position changed after approval; observe and approve again",
        ));
    }
    let x = (window.bounds.x
        + f64::from(matched.x + anchor.click.x) * window.bounds.width
            / f64::from(anchor.frame_width))
    .round();
    let y = (window.bounds.y
        + f64::from(matched.y + anchor.click.y) * window.bounds.height
            / f64::from(anchor.frame_height))
    .round();
    if x < f64::from(i32::MIN)
        || x > f64::from(i32::MAX)
        || y < f64::from(i32::MIN)
        || y > f64::from(i32::MAX)
        || !window.bounds.contains(x, y)
        || !expected.screen_bounds.contains(x, y)
    {
        return Err(failure(
            "outside_target",
            "Visual click must remain in the exact attached target",
        ));
    }
    Ok((x, y))
}

#[cfg(test)]
mod tests {
    use super::{
        MAX_COMPARISONS, PixelPoint, PixelRect, VisualAnchor, resolve_visual, revalidate_visual,
    };
    use crate::{Action, ActionKind, Bounds, Effect, Screenshot, Selector, Window};
    use base64::{Engine, engine::general_purpose::STANDARD};
    use image::{DynamicImage, ImageFormat, Rgba, RgbaImage};
    use sha2::{Digest, Sha256};
    use std::io::Cursor;

    fn png(image: &RgbaImage) -> Vec<u8> {
        let mut out = Cursor::new(Vec::new());
        DynamicImage::ImageRgba8(image.clone())
            .write_to(&mut out, ImageFormat::Png)
            .unwrap();
        out.into_inner()
    }
    fn fixture() -> (Window, Screenshot, VisualAnchor, RgbaImage, RgbaImage) {
        let mut template = RgbaImage::from_pixel(8, 8, Rgba([80, 120, 160, 255]));
        template.put_pixel(3, 4, Rgba([240, 20, 40, 255]));
        let mut frame = RgbaImage::from_pixel(64, 64, Rgba([0, 0, 0, 255]));
        image::imageops::replace(&mut frame, &template, 10, 12);
        let window = Window {
            pid: 42,
            window_id: 7,
            title: "fixture".into(),
            process_identity: "fixture-process".into(),
            bounds: Bounds {
                x: -4.,
                y: -10.,
                width: 32.,
                height: 32.,
            },
            foreground: true,
            scale: 2.,
        };
        let bytes = png(&template);
        let anchor = VisualAnchor {
            png_base64: STANDARD.encode(&bytes),
            sha256: format!("{:x}", Sha256::digest(&bytes)),
            width: 8,
            height: 8,
            frame_width: 64,
            frame_height: 64,
            window_width: 32.,
            window_height: 32.,
            scale: 2.,
            search: PixelRect {
                x: 0,
                y: 0,
                width: 64,
                height: 64,
            },
            click: PixelPoint { x: 4, y: 4 },
        };
        let capture = Screenshot {
            mime_type: "image/png".into(),
            base64: STANDARD.encode(png(&frame)),
            width: 64,
            height: 64,
        };
        (window, capture, anchor, frame, template)
    }
    fn action(target_id: String) -> Action {
        Action {
            action_id: "click".into(),
            observation_id: "o".into(),
            target_id,
            kind: ActionKind::Click,
            text: None,
            x: None,
            y: None,
            scroll_y: None,
            effect: Effect::Read,
            approval_id: Some("grant".into()),
        }
    }

    #[test]
    fn canonical_go_encoded_anchor_fixture_matches_native_rgba() {
        let anchor: VisualAnchor =
            serde_json::from_str(include_str!("../tests/fixtures/visual-anchor.json")).unwrap();
        let (window, capture, _, _, _) = fixture();
        let result = resolve_visual(&window, &capture, &anchor, "cross-language").unwrap();
        assert_eq!(result.anchor_sha256, anchor.sha256);
        assert_eq!(
            result.matched,
            PixelRect {
                x: 10,
                y: 12,
                width: 8,
                height: 8
            }
        );
    }

    #[test]
    fn unique_visual_match_binds_observation_and_maps_negative_origin_retina_pixels() {
        let (window, capture, anchor, _, _) = fixture();
        let matched = resolve_visual(&window, &capture, &anchor, "o").unwrap();
        assert_eq!(
            matched.matched,
            PixelRect {
                x: 10,
                y: 12,
                width: 8,
                height: 8
            }
        );
        assert_eq!(
            revalidate_visual(
                &window,
                &capture,
                &anchor,
                &matched,
                &action(matched.target_id.clone())
            )
            .unwrap(),
            (3., -2.)
        );
        assert_ne!(
            matched.target_id,
            resolve_visual(&window, &capture, &anchor, "next-observation")
                .unwrap()
                .target_id
        );
    }

    #[test]
    fn missing_and_duplicated_visual_anchors_are_distinct_refusals() {
        let (window, mut capture, anchor, mut frame, template) = fixture();
        image::imageops::replace(&mut frame, &template, 30, 30);
        capture.base64 = STANDARD.encode(png(&frame));
        assert_eq!(
            resolve_visual(&window, &capture, &anchor, "o")
                .unwrap_err()
                .code,
            "target_ambiguous"
        );
        frame.fill(0);
        capture.base64 = STANDARD.encode(png(&frame));
        assert_eq!(
            resolve_visual(&window, &capture, &anchor, "o")
                .unwrap_err()
                .code,
            "target_missing"
        );
    }

    #[test]
    fn changed_pixels_outside_anchor_invalidate_reviewed_visual_state() {
        let (window, mut capture, anchor, mut frame, _) = fixture();
        let matched = resolve_visual(&window, &capture, &anchor, "o").unwrap();
        frame.put_pixel(0, 0, Rgba([1, 0, 0, 255]));
        capture.base64 = STANDARD.encode(png(&frame));
        assert_eq!(
            revalidate_visual(
                &window,
                &capture,
                &anchor,
                &matched,
                &action(matched.target_id.clone())
            )
            .unwrap_err()
            .code,
            "visual_state_changed"
        );
    }

    #[test]
    fn visual_action_rejects_coordinate_override_and_non_click() {
        let (window, capture, anchor, _, _) = fixture();
        let matched = resolve_visual(&window, &capture, &anchor, "o").unwrap();
        let mut a = action(matched.target_id.clone());
        a.x = Some(2.);
        assert_eq!(
            revalidate_visual(&window, &capture, &anchor, &matched, &a)
                .unwrap_err()
                .code,
            "invalid_action"
        );
        a.x = None;
        a.kind = ActionKind::Press;
        assert_eq!(
            revalidate_visual(&window, &capture, &anchor, &matched, &a)
                .unwrap_err()
                .code,
            "invalid_action"
        );
    }

    #[test]
    fn visual_digest_dimensions_scale_and_mixed_selectors_fail_closed() {
        let (window, capture, mut anchor, _, _) = fixture();
        anchor.sha256 = "0".repeat(64);
        assert_eq!(
            resolve_visual(&window, &capture, &anchor, "o")
                .unwrap_err()
                .code,
            "visual_invalid"
        );
        let (_, _, mut anchor, _, _) = fixture();
        anchor.scale = 1.;
        assert_eq!(
            resolve_visual(&window, &capture, &anchor, "o")
                .unwrap_err()
                .code,
            "visual_geometry_changed"
        );
        let (_, _, mut anchor, _, _) = fixture();
        anchor.width = 9;
        assert_eq!(
            resolve_visual(&window, &capture, &anchor, "o")
                .unwrap_err()
                .code,
            "visual_invalid"
        );
        let (_, _, anchor, _, _) = fixture();
        assert!(
            Selector {
                name: Some("button".into()),
                visual: Some(anchor),
                ..Selector::default()
            }
            .validate()
            .is_err()
        );
    }

    #[test]
    fn invisible_or_uniform_visual_templates_are_rejected() {
        let (window, capture, mut anchor, _, mut template) = fixture();
        for color in [[1, 1, 1, 255], [1, 1, 1, 0]] {
            for pixel in template.pixels_mut() {
                *pixel = Rgba(color);
            }
            let bytes = png(&template);
            anchor.png_base64 = STANDARD.encode(&bytes);
            anchor.sha256 = format!("{:x}", Sha256::digest(bytes));
            assert_eq!(
                resolve_visual(&window, &capture, &anchor, "o")
                    .unwrap_err()
                    .code,
                "visual_invalid"
            );
        }
    }

    #[test]
    fn search_work_limit_is_not_reported_as_a_missing_or_unique_match() {
        let (mut window, mut capture, mut anchor, _, _) = fixture();
        let mut template = RgbaImage::from_pixel(128, 128, Rgba([0, 0, 0, 255]));
        template.put_pixel(127, 127, Rgba([1, 0, 0, 255]));
        let frame = RgbaImage::from_pixel(256, 256, Rgba([0, 0, 0, 255]));
        let bytes = png(&template);
        anchor.png_base64 = STANDARD.encode(&bytes);
        anchor.sha256 = format!("{:x}", Sha256::digest(bytes));
        anchor.width = 128;
        anchor.height = 128;
        anchor.frame_width = 256;
        anchor.frame_height = 256;
        anchor.window_width = 128.;
        anchor.window_height = 128.;
        anchor.search = PixelRect {
            x: 0,
            y: 0,
            width: 256,
            height: 256,
        };
        window.bounds.width = 128.;
        window.bounds.height = 128.;
        capture.width = 256;
        capture.height = 256;
        capture.base64 = STANDARD.encode(png(&frame));
        let worst_case = u64::from(anchor.search.width - anchor.width + 1)
            * u64::from(anchor.search.height - anchor.height + 1)
            * u64::from(anchor.width)
            * u64::from(anchor.height);
        assert!(worst_case > MAX_COMPARISONS);
        assert_eq!(
            resolve_visual(&window, &capture, &anchor, "o")
                .unwrap_err()
                .code,
            "visual_search_limit"
        );
    }
}
