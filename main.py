"""Validate PDF files in a directory and delete the corrupt ones."""

import logging  # Structured logging instead of print
import sys  # Exit codes
from dataclasses import dataclass, field  # Simple container for run results
from pathlib import Path  # Cleaner path handling than os.path

try:
    import pymupdf as fitz  # Modern PyMuPDF import name (no deprecation warning)
except ImportError:  # Older PyMuPDF versions only provide `fitz`
    import fitz  # Fall back to the legacy import name

PDF_DIRECTORY = Path("./PDFs")  # Directory scanned recursively for PDFs

logger = logging.getLogger("validate_pdfs")  # Module-level logger


@dataclass
class ValidationSummary:
    """Counts and file lists collected during a run."""

    checked: int = 0  # Number of PDFs inspected
    valid: int = 0  # Number of PDFs that opened correctly
    invalid: list[Path] = field(default_factory=list)  # Corrupt PDFs found
    deleted: int = 0  # Number of corrupt PDFs actually removed
    delete_errors: int = 0  # Number of failed deletions


def find_pdf_files(directory: Path) -> list[Path]:
    """Recursively find PDFs, matching the extension case-insensitively."""
    return sorted(
        path
        for path in directory.rglob("*")  # Walk the whole tree
        if path.is_file() and path.suffix.lower() == ".pdf"  # Match .pdf, .PDF, .Pdf
    )


def validate_pdf_file(file_path: Path) -> tuple[bool, str]:
    """Return (is_valid, reason). The reason is empty for valid files."""
    try:
        file_size = file_path.stat().st_size  # Size on disk in bytes
        logger.debug("Validating %s (%d bytes)", file_path, file_size)
        if file_size == 0:  # Zero-byte files can never be valid
            return False, "file is empty (0 bytes)"

        with fitz.open(file_path) as document:  # Context manager closes the file for us
            if document.needs_pass:  # Encrypted PDFs are not corrupt, just locked
                logger.debug("%s is password-protected; treating as valid", file_path)
                return True, ""
            if document.page_count == 0:  # A PDF with no pages is unusable
                return False, "no pages"
            logger.debug("%s has %d page(s)", file_path, document.page_count)
            document.load_page(
                0
            )  # Force-parse the first page to catch hidden corruption
        return True, ""  # Opened, has pages, first page loads
    except Exception as error:  # PyMuPDF raises several error types for bad files
        return (
            False,
            f"{type(error).__name__}: {error}",
        )  # Include the error type for easier debugging


def remove_file(file_path: Path) -> bool:
    """Delete a file, returning False instead of crashing if it fails."""
    try:
        file_path.unlink()  # Remove the file
        return True
    except OSError as error:  # Permission denied, file in use, already gone, etc.
        logger.error("Could not delete %s: %s", file_path, error)
        return False


def process_directory(directory: Path) -> ValidationSummary:
    """Validate every PDF under `directory` and delete the invalid ones."""
    summary = ValidationSummary()  # Start with empty counters
    logger.info("Scanning directory: %s", directory.resolve())
    pdf_files = find_pdf_files(
        directory
    )  # Collect all PDFs first so we can report the total
    logger.info("Found %d PDF file(s)", len(pdf_files))

    for index, pdf_path in enumerate(pdf_files, start=1):  # Loop over each PDF found
        logger.debug("Processing file %d of %d: %s", index, len(pdf_files), pdf_path)
        summary.checked += 1  # Count it
        is_valid, reason = validate_pdf_file(pdf_path)  # Inspect the file

        if is_valid:
            summary.valid += 1  # Count it as good
            logger.info("Valid PDF: %s", pdf_path)
            continue  # Nothing else to do for good files

        summary.invalid.append(pdf_path)  # Remember the bad file
        logger.warning("Invalid PDF: %s (%s)", pdf_path, reason)

        if remove_file(pdf_path):  # Try to delete the corrupt file
            summary.deleted += 1
            logger.info("Deleted: %s", pdf_path)
        else:
            summary.delete_errors += 1  # Deletion failed, already logged

    return summary


def main() -> int:
    """Entry point. Returns a process exit code."""
    logging.basicConfig(  # Send every log message to the console
        level=logging.DEBUG,  # Show everything, including per-file details
        format="%(asctime)s %(levelname)s: %(message)s",  # Timestamp each line
        stream=sys.stdout,  # Print to standard output
    )
    logger.info("Starting PDF validation")
    fitz.TOOLS.mupdf_display_errors(
        False
    )  # Silence raw MuPDF stderr noise; we log errors ourselves

    if not PDF_DIRECTORY.is_dir():  # Fail early on a wrong path
        logger.error("Directory not found: %s", PDF_DIRECTORY)
        return 2

    summary = process_directory(PDF_DIRECTORY)  # Do the work

    logger.info(  # Print a one-line summary
        "Checked %d | valid %d | invalid %d | deleted %d | delete errors %d",
        summary.checked,
        summary.valid,
        len(summary.invalid),
        summary.deleted,
        summary.delete_errors,
    )
    return 1 if summary.delete_errors else 0  # Non-zero exit if any deletion failed


if __name__ == "__main__":  # Only run when executed directly, not when imported
    sys.exit(main())
