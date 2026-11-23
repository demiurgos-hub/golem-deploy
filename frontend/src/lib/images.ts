import type { Image } from "@/api/types.gen";
import { shortImageId } from "@/lib/format";

/** A readable name for an image: its first tag, or its short id. */
export function imageName(img: Pick<Image, "id" | "tags">): string {
  return img.tags[0] ?? shortImageId(img.id);
}

/**
 * A server's image (always the stored image id) as a readable name, using
 * the managed image list when it is loaded.
 */
export function imageLabel(id: string, images: readonly Image[] | undefined): string {
  const img = images?.find((i) => i.id === id);
  return img ? imageName(img) : shortImageId(id);
}
