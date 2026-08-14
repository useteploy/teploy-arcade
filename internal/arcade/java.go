package arcade

import (
	"strconv"
	"strings"
)

// Choosing the JRE a Minecraft version actually runs on.
//
// Every Java-edition template names the image `itzg/minecraft-server` with no
// tag, which resolves to :latest - currently Java 21. That is right for a
// modern server and fatally wrong for an old one: Forge 1.12.2 will not start
// on anything past Java 8, and the failure surfaces as a stack trace inside the
// game's log rather than as anything the panel can explain.
//
// This is not something to leave to the operator. Someone importing a 1.12.2
// modpack is not thinking about JVM releases, and "pick the right image tag" is
// panel knowledge, not user knowledge. The tag is chosen from the version and
// can still be overridden per server for the cases this table gets wrong.
//
// The boundaries are Mojang's, not ours:
//
//	<= 1.16.5   Java 8   - 1.17 was the first release to require newer
//	1.17 - 1.20.4  Java 17
//	>= 1.20.5   Java 21  - the 1.20.5 snapshot moved the floor again
const (
	imageJava8  = "java8"
	imageJava17 = "java17"
	imageJava21 = "java21"
	imageJava25 = "java25"
)

// parseMCVersion pulls the numeric parts out of a Minecraft version string.
// Anything it cannot read reports ok=false, and the caller keeps the default -
// guessing an old JRE for an unreadable version would break new servers to
// protect old ones.
func parseMCVersion(v string) (major, minor, patch int, ok bool) {
	v = strings.TrimSpace(v)
	// Trim a snapshot/pre-release suffix: "1.20.4-pre1", "1.21.1+build.3".
	for _, cut := range []string{"-", "+", " "} {
		if i := strings.Index(v, cut); i > 0 {
			v = v[:i]
		}
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, 0, false
	}
	nums := make([]int, 0, 3)
	for i, p := range parts {
		if i == 3 {
			break
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, 0, false
		}
		nums = append(nums, n)
	}
	for len(nums) < 3 {
		nums = append(nums, 0)
	}
	return nums[0], nums[1], nums[2], true
}

// javaTagFor reports the itzg image tag whose JRE the version runs on, and
// whether a tag could be determined at all.
func javaTagFor(version string) (string, bool) {
	major, minor, patch, ok := parseMCVersion(version)
	if !ok {
		return "", false
	}
	// Minecraft left the 1.x scheme behind: releases are now year-based
	// (26.1.2). Those are newer than every boundary below, so they take the
	// newest JRE we know of. Without this the whole table fell through to the
	// untagged image, which happens to ship Java 25 today - the right answer by
	// accident, and only until that tag moves.
	//
	// Floored at 20 rather than "anything not 1.x": there will never be a
	// Minecraft 2.x through 19.x, so a version in that range is not a release
	// this table understands (1.RV and the 2.0 April Fools build both land
	// there) and gets no tag rather than a guessed one.
	if major >= 20 {
		return imageJava25, true
	}
	if major != 1 {
		return "", false
	}
	switch {
	case minor <= 16:
		return imageJava8, true
	case minor < 20:
		return imageJava17, true
	case minor == 20 && patch < 5:
		return imageJava17, true
	default:
		return imageJava21, true
	}
}

// imageForVersion applies javaTagFor to a template's image.
//
// It only ever adds a tag to the untagged itzg Java-edition image. An image the
// operator has already pinned, a custom registry, or any other image is left
// exactly as it is: overriding an explicit choice is how a panel earns a
// reputation for doing things behind your back.
func imageForVersion(image, version string) string {
	const base = "itzg/minecraft-server"
	if image != base {
		return image
	}
	tag, ok := javaTagFor(version)
	if !ok {
		return image
	}
	return base + ":" + tag
}
